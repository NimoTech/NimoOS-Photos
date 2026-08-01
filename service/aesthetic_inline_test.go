package service

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/aesthetic"
	"github.com/stretchr/testify/require"
)

// loadTestHead hand-assembles bytes in the NAES format, building a
// single-layer linear head (len(w)→1), handed to aesthetic.LoadFrom to
// parse. Test-only, doesn't go through the production API.
func loadTestHead(t *testing.T, w []float32, bias float32) *aesthetic.Head {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("NAES")
	ver := "v-test"
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(ver))))
	buf.WriteString(ver)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1))) // nLayers=1
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(w))))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1))) // out=1
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, w))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, []float32{bias}))
	h, err := aesthetic.LoadFrom(&buf)
	require.NoError(t, err)
	return h
}

// insertIndexedAsset inserts a minimal assets row, so writeClipEmbedding/
// scoreAesthetic's UPDATE has a target to exist against.
func insertIndexedAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?, 'indexed')`,
		id, "/x/"+id+".jpg")
	require.NoError(t, err)
}

// writeClipEmbedding should write aesthetic_score inline on success; it doesn't write when no head is injected (stays NULL).
func TestWriteClipEmbeddingInlineAesthetic(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	insertIndexedAsset(t, db, "a1")

	// 1152-dim single-layer head: W all 0, b=7.5 ⇒ any vector scores 7.5, making assertions easy.
	w := make([]float32, common.CLIPDim)
	head := loadTestHead(t, w, 7.5)
	ix.SetAestheticHead(head)

	vec := make([]float32, common.CLIPDim)
	vec[0] = 1
	require.NoError(t, ix.writeClipEmbedding("a1", vec))

	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.True(t, score.Valid)
	require.InDelta(t, 7.5, score.Float64, 1e-6)
}

// When no aesthetic head is injected, writeClipEmbedding still succeeds but aesthetic_score stays NULL.
func TestWriteClipEmbeddingNoHeadLeavesNull(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	insertIndexedAsset(t, db, "a1")

	vec := make([]float32, common.CLIPDim)
	vec[0] = 1
	require.NoError(t, ix.writeClipEmbedding("a1", vec))

	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.False(t, score.Valid)
}
