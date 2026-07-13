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

// loadTestHead 用 NAES 格式手工拼字节,拼出一个单层线性头(len(w)→1),
// 交给 aesthetic.LoadFrom 解析。仅供测试用,不走生产 API。
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

// insertIndexedAsset 插入一条最小 assets 行,供 writeClipEmbedding/scoreAesthetic
// 的 UPDATE 目标存在。
func insertIndexedAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?, 'indexed')`,
		id, "/x/"+id+".jpg")
	require.NoError(t, err)
}

// writeClipEmbedding 成功后应内联写 aesthetic_score;未注入头时不写(保持 NULL)。
func TestWriteClipEmbeddingInlineAesthetic(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	insertIndexedAsset(t, db, "a1")

	// 1152 维单层头:W 全 0、b=7.5 ⇒ 任何向量得 7.5,便于断言。
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

// 未注入美学头时,writeClipEmbedding 成功但 aesthetic_score 保持 NULL。
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
