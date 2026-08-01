package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// semML's CLIPTextEmbed returns a distinguishable unit vector per prompt group:
// doc group → e0, photo group → e1. The image vector is inserted by the test
// itself; close to e0 means semantically a document, close to e1 a photo.
type semML struct{ mockML }

func (m *semML) CLIPTextEmbed(text string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	for _, p := range photoPrompts {
		if p == text {
			v[1] = 1
			return v, nil
		}
	}
	v[0] = 1
	return v, nil
}

// seedDocAsset inserts an asset with an image vector + OCR lines. vecDir=0 →
// aligned with the doc prompts, 1 → aligned with the photo prompts.
func seedDocAsset(t *testing.T, ix *Indexer, id string, vecDir int, boxes [][]float64) {
	t.Helper()
	_, err := ix.db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
	require.NoError(t, err)
	_, err = ix.db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,boxes_ver) VALUES(?, 'x', 0.1, ?, 1)`, id, len(boxes))
	require.NoError(t, err)
	for i, b := range boxes {
		blob, merr := json.Marshal(b)
		require.NoError(t, merr)
		_, err = ix.db.Exec(`INSERT INTO asset_ocr_lines(asset_id,line_no,text,box,score) VALUES(?,?,?,?,0.9)`, id, i, "line", string(blob))
		require.NoError(t, err)
	}
	vec := make([]float32, common.CLIPDim)
	vec[vecDir] = 1
	_, err = ix.db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, ix.db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
	_, err = ix.db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

func TestComputeDocVerdict(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &semML{}, t.TempDir(), 1)

	seedDocAsset(t, ix, "doc1", 0, regularBoxes(10))   // semantically a document + regular geometry
	seedDocAsset(t, ix, "photo1", 1, scatteredBoxes()) // semantically a photo + scattered geometry

	require.NoError(t, ix.computeDocVerdict("doc1"))
	require.NoError(t, ix.computeDocVerdict("photo1"))

	var isDoc, docVer int
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver FROM asset_ocr WHERE asset_id='doc1'`).Scan(&isDoc, &docVer))
	require.Equal(t, 1, isDoc)
	require.Equal(t, 1, docVer)
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver FROM asset_ocr WHERE asset_id='photo1'`).Scan(&isDoc, &docVer))
	require.Equal(t, 0, isDoc, "semantically a photo + scattered geometry should be rejected")

	// Prompt vectors should now be in the DB cache
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_text_cache WHERE gen=?`, common.MLModelGen).Scan(&n))
	require.Equal(t, len(docPrompts)+len(photoPrompts), n)
}

// offlineML: text embedding is unavailable (simulates ML being offline).
type offlineML struct{ mockML }

func (m *offlineML) CLIPTextEmbed(_ string) ([]float32, error) {
	return nil, errors.New("ml offline")
}
func (m *offlineML) IsReady() bool { return false }

func TestComputeDocVerdictMLOffline(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &offlineML{}, t.TempDir(), 1)
	seedDocAsset(t, ix, "a1", 0, regularBoxes(5))

	require.NoError(t, ix.computeDocVerdict("a1"), "ML being offline is not an error")

	var isDoc *int
	var docVer int
	var docGeo *float64
	require.NoError(t, db.QueryRow(`SELECT is_doc, doc_ver, doc_geo FROM asset_ocr WHERE asset_id='a1'`).Scan(&isDoc, &docVer, &docGeo))
	require.Nil(t, isDoc, "is_doc stays NULL pending backfill")
	require.Equal(t, 0, docVer, "doc_ver stays 0")
	require.NotNil(t, docGeo, "geometry score doesn't depend on ML, persisted first")
}

func TestComputeDocVerdictNoVector(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &semML{}, t.TempDir(), 1)
	// OCR lines only, no image vector
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,boxes_ver) VALUES('a1','x',0.1,5,1)`)
	require.NoError(t, err)

	require.NoError(t, ix.computeDocVerdict("a1"))
	var docVer int
	require.NoError(t, db.QueryRow(`SELECT doc_ver FROM asset_ocr WHERE asset_id='a1'`).Scan(&docVer))
	require.Equal(t, 0, docVer, "no vector: leave for backfill")
}

func TestBackfillDocVerdicts(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &semML{}, t.TempDir(), 1)
	seedDocAsset(t, ix, "doc1", 0, regularBoxes(10))
	// An asset with no vector: must not be picked up, and must not block the run
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('novec','/p/n.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,boxes_ver) VALUES('novec','x',0.1,5,1)`)
	require.NoError(t, err)

	e := NewEmbedder(db, &semML{}, ix, nil)
	require.NoError(t, e.BackfillDocVerdicts(context.Background()))

	var docVer int
	require.NoError(t, db.QueryRow(`SELECT doc_ver FROM asset_ocr WHERE asset_id='doc1'`).Scan(&docVer))
	require.Equal(t, 1, docVer, "the one with a vector converges via backfill")
	require.NoError(t, db.QueryRow(`SELECT doc_ver FROM asset_ocr WHERE asset_id='novec'`).Scan(&docVer))
	require.Equal(t, 0, docVer, "the one without a vector is skipped, left for a later round once its vector arrives")

	// Idempotent: running another round with no changes must not error
	require.NoError(t, e.BackfillDocVerdicts(context.Background()))
}
