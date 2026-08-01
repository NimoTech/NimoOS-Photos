package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/aesthetic"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// buildTestAestheticHead hand-assembles an aesthetic-head byte stream
// (format documented in the pkg/aesthetic package comment): a single layer
// CLIPDim→1, all weights 0, bias=7, so any image vector scores the constant
// 7, making assertions easy.
func buildTestAestheticHead(t *testing.T) *aesthetic.Head {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("NAES")
	ver := "v-test"
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(ver))))
	buf.WriteString(ver)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))              // nLayers
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(common.CLIPDim))) // in
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))              // out
	weights := make([]float32, common.CLIPDim)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, weights))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, []float32{7}))
	h, err := aesthetic.LoadFrom(&buf)
	require.NoError(t, err)
	return h
}

// insertAssetWithClipVec inserts a status='indexed' asset and writes a CLIPDim-sized image vector for it.
func insertAssetWithClipVec(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
	vec := make([]float32, common.CLIPDim)
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

// TestBackfillAesthetic: an asset with a vector but no score gets scored;
// one without a vector is skipped and left NULL; an asset that already has
// a score should not be recomputed; a done task should show up in the TaskRegistry.
func TestBackfillAesthetic(t *testing.T) {
	db := makeTestDB(t)
	head := buildTestAestheticHead(t)

	insertAssetWithClipVec(t, db, "a1") // has a vector, no score → should be scored

	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a2','/p/a2.jpg','indexed')`)
	require.NoError(t, err) // no vector → not selected, stays NULL

	insertAssetWithClipVec(t, db, "a3") // has a vector, pre-set a score manually → should not be recomputed
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=? WHERE id='a3'`, 3.5)
	require.NoError(t, err)

	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(tk Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	e := NewEmbedder(db, &mockML{}, nil, reg)
	e.SetAestheticHead(head)

	require.NoError(t, e.BackfillAesthetic(context.Background()))

	var s1 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&s1))
	require.True(t, s1.Valid, "a1 has a vector and no score, should have a score after backfill")
	require.InDelta(t, 7, s1.Float64, 1e-6)

	var s2 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a2'`).Scan(&s2))
	require.False(t, s2.Valid, "a2 has no vector, should stay NULL")

	var s3 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a3'`).Scan(&s3))
	require.True(t, s3.Valid)
	require.InDelta(t, 3.5, s3.Float64, 1e-6, "a3 already has a score, should not be recomputed")

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "aesthetic" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv, "there should be a done task with type==aesthetic")
}

// TestBackfillAestheticCAS verifies the concurrency guard: a second call
// received while running should return nil immediately and set
// aestheticRerunPending; once the current pass ends, pending is
// automatically consumed and another pass runs, eventually filling
// everything in, without panicking (following the style of
// embedder_test.go's TestEmbedder_BackfillOCRRerunPendingConsumedAfterRun).
func TestBackfillAestheticCAS(t *testing.T) {
	db := makeTestDB(t)
	head := buildTestAestheticHead(t)
	insertAssetWithClipVec(t, db, "a1")

	e := NewEmbedder(db, &mockML{}, nil, NewTaskRegistry(nil))
	e.SetAestheticHead(head)

	// Simulate an aesthetic backfill round already running: a trigger arriving now must set pending, not be swallowed.
	e.aestheticRunning.Store(true)
	require.NoError(t, e.BackfillAesthetic(context.Background()))
	require.True(t, e.aestheticRerunPending.Load(), "a trigger received while running must set aestheticRerunPending")
	e.aestheticRunning.Store(false)

	// The next actual run consumes pending and runs one more round (the second round is a no-op on an empty backlog).
	require.NoError(t, e.BackfillAesthetic(context.Background()))
	require.False(t, e.aestheticRerunPending.Load(), "pending should be consumed once the backfill finishes")

	var s1 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&s1))
	require.True(t, s1.Valid, "everything should be filled in eventually")
}

// TestEnsureAestheticHeadVer: on a version mismatch, the whole DB is set to
// NULL and the new version stamped; when versions match (idempotent), scores are untouched.
func TestEnsureAestheticHeadVer(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,aesthetic_score) VALUES('a1','/p/a1.jpg','indexed',5)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO photos_meta(key,value) VALUES('aesthetic_head_ver','old')`)
	require.NoError(t, err)

	require.NoError(t, EnsureAestheticHeadVer(db, "new"))

	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.False(t, score.Valid, "a version mismatch should NULL out the whole DB")

	var ver string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='aesthetic_head_ver'`).Scan(&ver))
	require.Equal(t, "new", ver)

	// Idempotent: scores should not be cleared again when the version matches.
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=6 WHERE id='a1'`)
	require.NoError(t, err)
	require.NoError(t, EnsureAestheticHeadVer(db, "new"))

	var score2 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score2))
	require.True(t, score2.Valid, "scores should not be cleared when the version matches")
	require.InDelta(t, 6, score2.Float64, 1e-6)
}
