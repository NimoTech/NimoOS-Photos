package service_test

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// makeTestFaceDB opens a fresh temp SQLite database for face tests.
func makeTestFaceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "fc.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

func TestDBSCAN_TwoClusters(t *testing.T) {
	dim := 512
	v1, v2, v3 := make([]float32, dim), make([]float32, dim), make([]float32, dim)
	v1[0] = 1.0
	v2[0] = 0.99  // 接近 v1，余弦距离 ≈ 0
	v3[1] = 1.0   // 与 v1 正交，余弦距离 = 1.0

	labels := service.DBSCAN([][]float32{normalize(v1), normalize(v2), normalize(v3)}, 0.4, 1)
	require.Len(t, labels, 3)
	require.Equal(t, labels[0], labels[1], "v1 和 v2 应在同一 cluster")
	require.NotEqual(t, labels[0], labels[2], "v3 应在不同 cluster")
	require.GreaterOrEqual(t, labels[0], 0)
	require.GreaterOrEqual(t, labels[2], 0)
}

func TestDBSCAN_SingletonClusters(t *testing.T) {
	dim := 512
	v1, v2 := make([]float32, dim), make([]float32, dim)
	v1[0] = 1.0
	v2[1] = 1.0  // 正交，余弦距离 = 1.0 > 0.4
	labels := service.DBSCAN([][]float32{normalize(v1), normalize(v2)}, 0.4, 1)
	require.NotEqual(t, labels[0], labels[1])
	require.GreaterOrEqual(t, labels[0], 0)
	require.GreaterOrEqual(t, labels[1], 0)
}

func TestDBSCAN_Empty(t *testing.T) {
	labels := service.DBSCAN([][]float32{}, 0.6, 1)
	require.Empty(t, labels)
}

func TestRunClustering(t *testing.T) {
	db := makeTestFaceDB(t)

	// 插入 2 个相似人脸（应聚成 1 个 person）+ 1 个不相似（单独 person）
	// 人脸 1 和 2：v[0]=1，归一化后余弦距离 ≈ 0
	// 人脸 3：v[1]=1，正交

	db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p1.jpg','indexed')`)
	db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a2','/p2.jpg','indexed')`)

	dim := 512
	face := func(val float32, idx int) []float32 {
		v := make([]float32, dim)
		v[idx] = val
		return v
	}

	insert := func(faceID, assetID string, vec []float32) {
		db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',?)`,
			faceID, assetID, sqlite.SerializeFloat32(vec))
	}
	insert("f1", "a1", face(1.0, 0))
	insert("f2", "a1", face(0.9999, 0)) // 极相似
	insert("f3", "a2", face(1.0, 1))    // 正交

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount)
	require.Equal(t, 2, personCount, "应有 2 个 person（1 个双脸，1 个单脸）")

	var fpCount int
	db.QueryRow(`SELECT COUNT(*) FROM face_person`).Scan(&fpCount)
	require.Equal(t, 3, fpCount, "所有 3 个 face 都应有 person 关联")
}

// TestRunClustering_ConcurrencyGuard 同时启两个 RunClustering，
// 期待第二个秒返回 nil，且 persons / face_person 状态只被一次操作改写。
func TestRunClustering_ConcurrencyGuard(t *testing.T) {
	db := makeTestFaceDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a','/a.jpg','indexed')`)
	require.NoError(t, err)
	for i := 0; i < 60; i++ {
		vec := make([]float32, 512)
		vec[i%512] = 1
		_, err := db.Exec(`
            INSERT INTO face_detections(id, asset_id, bbox, embedding)
            VALUES(?, 'a', '{}', ?)`,
			uuid.NewString(), sqlite.SerializeFloat32(vec),
		)
		require.NoError(t, err)
	}

	s := service.NewFaceService(db)
	var wg sync.WaitGroup
	var errs [2]error
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = s.RunClustering(context.Background()) }()
	go func() { defer wg.Done(); errs[1] = s.RunClustering(context.Background()) }()
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	var personCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount)
	require.Equal(t, 60, personCount, "60 face、minPts=1 应得 60 簇/60 persons")
}

// TestRunClustering_EmptyDoesNotPublish 0 人脸时不发任何 task。
func TestRunClustering_EmptyDoesNotPublish(t *testing.T) {
	db := makeTestFaceDB(t)
	var emitted []service.Task
	var mu sync.Mutex
	s := service.NewFaceService(db)
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)
	require.NoError(t, s.RunClustering(context.Background()))
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, emitted)
}

// TestRunClustering_StagesPublishProgress 断言三阶段都发出 progress。
func TestRunClustering_StagesPublishProgress(t *testing.T) {
	db := makeTestFaceDB(t)
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a','/a.jpg','indexed')`)
	for i := 0; i < 5; i++ {
		vec := make([]float32, 512)
		vec[i] = 1
		_, _ = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?, 'a', '{}', ?)`,
			uuid.NewString(), sqlite.SerializeFloat32(vec))
	}
	var emitted []service.Task
	var mu sync.Mutex
	s := service.NewFaceService(db)
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)
	require.NoError(t, s.RunClustering(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	var sawLoading, sawClustering, sawPersisting, sawDone bool
	for _, e := range emitted {
		if e.Type != "face" {
			continue
		}
		switch {
		case e.Status == "done" && e.Progress == 1:
			sawDone = true
		case e.Progress > 0 && e.Progress < 0.10:
			sawLoading = true
		case e.Progress >= 0.10 && e.Progress < 0.85:
			sawClustering = true
		case e.Progress >= 0.85 && e.Progress < 1:
			sawPersisting = true
		}
	}
	require.True(t, sawLoading, "应有 loading 阶段 progress")
	require.True(t, sawClustering, "应有 clustering 阶段 progress")
	require.True(t, sawPersisting, "应有 persisting 阶段 progress")
	require.True(t, sawDone, "应有 done event")
}

// TestDBSCANWithProgress 断言 onProgress 回调被调用，progress 单调递增，
// 最后一次回调 done == n。
func TestDBSCANWithProgress(t *testing.T) {
	// 50 个完全分离的点，每个独立成簇（minPts=1）。
	vecs := make([][]float32, 50)
	for i := range vecs {
		v := make([]float32, 512)
		v[i] = 1
		vecs[i] = v
	}

	var calls [][2]int
	labels := service.DBSCANWithProgress(vecs, 0.6, 1, func(done, n int) {
		calls = append(calls, [2]int{done, n})
	})

	require.Equal(t, 50, len(labels))
	require.NotEmpty(t, calls, "应至少触发一次回调")
	require.Equal(t, [2]int{50, 50}, calls[len(calls)-1], "最后一次回调应是 done==n")

	// 单调递增
	for i := 1; i < len(calls); i++ {
		require.GreaterOrEqual(t, calls[i][0], calls[i-1][0], "done 应单调递增")
	}
}

func TestComputeCentroidAndConfidence(t *testing.T) {
	dim := 512
	v1 := make([]float32, dim)
	v2 := make([]float32, dim)
	v1[0] = 1.0
	v2[0] = 0.98
	v2[1] = 0.02
	vecs := [][]float32{normalize(v1), normalize(v2)}

	c := service.ComputeCentroid(vecs)
	require.Len(t, c, dim)

	conf := service.ClusterConfidence(vecs, c)
	require.Greater(t, conf, 0.9, "近似向量簇置信度应高")
	require.LessOrEqual(t, conf, 1.0)

	// 单元素簇置信度为 1.0
	require.Equal(t, 1.0, service.ClusterConfidence([][]float32{normalize(v1)}, normalize(v1)))
	// 空簇安全
	require.Equal(t, 0.0, service.ClusterConfidence(nil, nil))

	// 对立向量簇置信度应低
	v3 := make([]float32, dim)
	v3[0] = -1.0
	vecs2 := [][]float32{normalize(v1), normalize(v3)}
	c2 := service.ComputeCentroid(vecs2)
	conf2 := service.ClusterConfidence(vecs2, c2)
	require.Less(t, conf2, 0.1, "对立向量簇置信度应低")

	// 维度不一致返回 nil
	require.Nil(t, service.ComputeCentroid([][]float32{make([]float32, 4), make([]float32, 5)}))
}
