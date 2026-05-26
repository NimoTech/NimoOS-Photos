package service_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

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
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "fc.db"))
	require.NoError(t, err)
	defer db.Close()

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
	require.NoError(t, svc.RunClustering())

	var personCount int
	db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount)
	require.Equal(t, 2, personCount, "应有 2 个 person（1 个双脸，1 个单脸）")

	var fpCount int
	db.QueryRow(`SELECT COUNT(*) FROM face_person`).Scan(&fpCount)
	require.Equal(t, 3, fpCount, "所有 3 个 face 都应有 person 关联")
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
