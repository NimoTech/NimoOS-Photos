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

// buildTestAestheticHead 手工拼一个美学头字节流(格式见 pkg/aesthetic 包注释):
// 单层 CLIPDim→1,权重全 0、偏置=7,任何图向量都打分为常数 7,便于断言。
func buildTestAestheticHead(t *testing.T) *aesthetic.Head {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("NAES")
	ver := "v-test"
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(ver))))
	buf.WriteString(ver)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1))) // nLayers
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(common.CLIPDim))) // in
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))              // out
	weights := make([]float32, common.CLIPDim)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, weights))
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, []float32{7}))
	h, err := aesthetic.LoadFrom(&buf)
	require.NoError(t, err)
	return h
}

// insertAssetWithClipVec 插入一个 status='indexed' 的资产并写入 CLIPDim 维图向量。
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

// TestBackfillAesthetic:有向量无分的资产被补分;无向量的跳过留 NULL;
// 已有分的资产不应被重算;done 任务应出现在 TaskRegistry。
func TestBackfillAesthetic(t *testing.T) {
	db := makeTestDB(t)
	head := buildTestAestheticHead(t)

	insertAssetWithClipVec(t, db, "a1") // 有向量、无分 → 应被补分

	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a2','/p/a2.jpg','indexed')`)
	require.NoError(t, err) // 无向量 → 不入选,留 NULL

	insertAssetWithClipVec(t, db, "a3") // 有向量,手工先写已有分 → 不应重算
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
	require.True(t, s1.Valid, "a1 有向量无分,补跑后应有分")
	require.InDelta(t, 7, s1.Float64, 1e-6)

	var s2 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a2'`).Scan(&s2))
	require.False(t, s2.Valid, "a2 无向量,应留 NULL")

	var s3 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a3'`).Scan(&s3))
	require.True(t, s3.Valid)
	require.InDelta(t, 3.5, s3.Float64, 1e-6, "a3 已有分,不应被重算")

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "aesthetic" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv, "应有 type==aesthetic 的 done 任务")
}

// TestBackfillAestheticCAS 验证并发防重:运行中收到第二次调用应立即返回 nil 且
// 置 aestheticRerunPending;当前轮结束后自动消费 pending 再跑一轮,最终全部补齐,
// 不 panic(照 embedder_test.go TestEmbedder_BackfillOCRRerunPendingConsumedAfterRun 样式)。
func TestBackfillAestheticCAS(t *testing.T) {
	db := makeTestDB(t)
	head := buildTestAestheticHead(t)
	insertAssetWithClipVec(t, db, "a1")

	e := NewEmbedder(db, &mockML{}, nil, NewTaskRegistry(nil))
	e.SetAestheticHead(head)

	// 模拟一轮美学补跑正在运行:此时的触发必须置 pending 而不是被吞。
	e.aestheticRunning.Store(true)
	require.NoError(t, e.BackfillAesthetic(context.Background()))
	require.True(t, e.aestheticRerunPending.Load(), "运行中收到的触发必须置 aestheticRerunPending")
	e.aestheticRunning.Store(false)

	// 下一次真正运行结束时消费 pending 并再跑一轮(第二轮空库上是空跑)。
	require.NoError(t, e.BackfillAesthetic(context.Background()))
	require.False(t, e.aestheticRerunPending.Load(), "补跑结束后 pending 应被消费")

	var s1 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&s1))
	require.True(t, s1.Valid, "最终应全部补齐")
}

// TestEnsureAestheticHeadVer:版本不符时全库置 NULL 并盖章;版本一致时(幂等)不动分数。
func TestEnsureAestheticHeadVer(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,aesthetic_score) VALUES('a1','/p/a1.jpg','indexed',5)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO photos_meta(key,value) VALUES('aesthetic_head_ver','old')`)
	require.NoError(t, err)

	require.NoError(t, EnsureAestheticHeadVer(db, "new"))

	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.False(t, score.Valid, "版本不符应全库置 NULL")

	var ver string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='aesthetic_head_ver'`).Scan(&ver))
	require.Equal(t, "new", ver)

	// 幂等:版本一致时不应再清分数。
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=6 WHERE id='a1'`)
	require.NoError(t, err)
	require.NoError(t, EnsureAestheticHeadVer(db, "new"))

	var score2 sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score2))
	require.True(t, score2.Valid, "版本一致时不应清分数")
	require.InDelta(t, 6, score2.Float64, 1e-6)
}
