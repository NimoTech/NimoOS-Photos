package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// pipelineMockML implements service.MLProvider for RunPipeline tests.
// DetectAndRecognizeFaces returns one canned face per call unless faceErr is set
// (simulating an ML failure) or the image data matches a registered "fail" marker.
type pipelineMockML struct {
	mu       sync.Mutex
	calls    []string // image contents seen, in call order
	faceErr  error    // when set, DetectAndRecognizeFaces always returns this error
	facesPer int      // faces to return per successful call (default 1 when 0)
}

func (m *pipelineMockML) CLIPImageEmbed(_ []byte) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *pipelineMockML) CLIPTextEmbed(_ string) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *pipelineMockML) DetectAndRecognizeFaces(data []byte) ([]mlclient.FaceResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, string(data))
	m.mu.Unlock()
	if m.faceErr != nil {
		return nil, m.faceErr
	}
	n := m.facesPer
	if n == 0 {
		n = 1
	}
	out := make([]mlclient.FaceResult, n)
	for i := range out {
		vec := make([]float32, common.FaceDim)
		vec[i%common.FaceDim] = 1
		out[i] = mlclient.FaceResult{BBox: mlclient.BoundingBox{X1: 0, Y1: 0, X2: 1, Y2: 1}, Embedding: vec}
	}
	return out, nil
}
func (m *pipelineMockML) OCR(_ []byte) ([]mlclient.OCRLine, error) { return nil, nil }
func (m *pipelineMockML) IsReady() bool                            { return true }

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
	v2[0] = 0.99 // close to v1, cosine distance ≈ 0
	v3[1] = 1.0  // orthogonal to v1, cosine distance = 1.0

	labels := service.DBSCAN([][]float32{normalize(v1), normalize(v2), normalize(v3)}, 0.4, 1)
	require.Len(t, labels, 3)
	require.Equal(t, labels[0], labels[1], "v1 and v2 should be in the same cluster")
	require.NotEqual(t, labels[0], labels[2], "v3 should be in a different cluster")
	require.GreaterOrEqual(t, labels[0], 0)
	require.GreaterOrEqual(t, labels[2], 0)
}

func TestDBSCAN_SingletonClusters(t *testing.T) {
	dim := 512
	v1, v2 := make([]float32, dim), make([]float32, dim)
	v1[0] = 1.0
	v2[1] = 1.0 // orthogonal, cosine distance = 1.0 > 0.4
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

	// Insert 2 similar faces (should cluster into 1 person) + 1 dissimilar one
	// (its own person).
	// Faces 1 and 2: v[0]=1, cosine distance ≈ 0 after normalizing.
	// Face 3: v[1]=1, orthogonal.

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
	insert("f2", "a1", face(0.9999, 0)) // near-identical
	insert("f3", "a2", face(1.0, 1))    // orthogonal

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount)
	require.Equal(t, 2, personCount, "should have 2 persons (1 with two faces, 1 with one face)")

	var fpCount int
	db.QueryRow(`SELECT COUNT(*) FROM face_person`).Scan(&fpCount)
	require.Equal(t, 3, fpCount, "all 3 faces should have a person association")
}

// TestRunClustering_ConcurrencyGuard starts two RunClustering calls at the
// same time; expects the second to return nil immediately, and persons /
// face_person state to be rewritten by only one of the two operations.
func TestRunClustering_ConcurrencyGuard(t *testing.T) {
	db := makeTestFaceDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a','/a.jpg','indexed')`)
	require.NoError(t, err)
	for i := 0; i < 60; i++ {
		vec := make([]float32, common.FaceDim)
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
	require.Equal(t, 60, personCount, "60 faces, minPts=1 should yield 60 clusters/60 persons")
}

// TestRunClustering_EmptyDoesNotPublish asserts no task is published with 0 faces.
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

// TestRunClustering_StagesPublishProgress asserts all three stages emit progress.
func TestRunClustering_StagesPublishProgress(t *testing.T) {
	db := makeTestFaceDB(t)
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a','/a.jpg','indexed')`)
	for i := 0; i < 5; i++ {
		vec := make([]float32, common.FaceDim)
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
	require.True(t, sawLoading, "should have progress from the loading stage")
	require.True(t, sawClustering, "should have progress from the clustering stage")
	require.True(t, sawPersisting, "should have progress from the persisting stage")
	require.True(t, sawDone, "should have a done event")
}

// TestDBSCANWithProgress asserts the onProgress callback is invoked, progress
// is monotonically non-decreasing, and the last callback has done == n.
func TestDBSCANWithProgress(t *testing.T) {
	// 50 fully separated points, each forming its own cluster (minPts=1).
	vecs := make([][]float32, 50)
	for i := range vecs {
		v := make([]float32, common.FaceDim)
		v[i] = 1
		vecs[i] = v
	}

	var calls [][2]int
	labels := service.DBSCANWithProgress(vecs, 0.6, 1, func(done, n int) {
		calls = append(calls, [2]int{done, n})
	})

	require.Equal(t, 50, len(labels))
	require.NotEmpty(t, calls, "should trigger the callback at least once")
	require.Equal(t, [2]int{50, 50}, calls[len(calls)-1], "the last callback should have done==n")

	// Monotonically non-decreasing
	for i := 1; i < len(calls); i++ {
		require.GreaterOrEqual(t, calls[i][0], calls[i-1][0], "done should be monotonically non-decreasing")
	}
}

// TestRunPipeline_DetectsAndClusters covers TDD case (1): two assets with
// face_scanned=0; after RunPipeline, face_detections has rows, face_scanned=1,
// and the task is done with Total=2.
func TestRunPipeline_DetectsAndClusters(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()

	p1 := filepath.Join(dir, "a1.jpg")
	p2 := filepath.Join(dir, "a2.jpg")
	require.NoError(t, os.WriteFile(p1, []byte("img-a1"), 0o644))
	require.NoError(t, os.WriteFile(p2, []byte("img-a2"), 0o644))

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1', ?, 'indexed'), ('a2', ?, 'indexed')`, p1, p2)
	require.NoError(t, err)

	ml := &pipelineMockML{}
	s := service.NewFaceService(db)
	s.SetML(ml)

	var emitted []service.Task
	var mu sync.Mutex
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)

	require.NoError(t, s.RunPipeline(context.Background()))

	var faceCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections`).Scan(&faceCount))
	require.Equal(t, 2, faceCount, "each of the two assets should detect 1 face")

	scanned := map[string]int{}
	rows, err := db.Query(`SELECT id, face_scanned FROM assets`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var fs int
		require.NoError(t, rows.Scan(&id, &fs))
		scanned[id] = fs
	}
	rows.Close()
	require.Equal(t, map[string]int{"a1": 1, "a2": 1}, scanned)

	mu.Lock()
	defer mu.Unlock()
	var sawDone bool
	for _, e := range emitted {
		if e.Type == "face" && e.Status == "done" {
			sawDone = true
			require.Equal(t, int64(2), e.Total, "the done event's Total should be the asset count awaiting detection: 2")
		}
	}
	require.True(t, sawDone, "should have a face done event")
}

// TestRunPipeline_TailClusteringDoesNotFillCurrentTotal covers a point the
// final review specifically flagged: the running intermediate states of the
// clustering tail (95%→100% after detection completes) must not fill in
// Current/Total (must be zeroed). If current=processed and
// total=len(targets) were still filled in (the two are equal by this point),
// the frontend NimoTaskBar would prefer current/total to compute the
// percentage whenever total>0, causing the tail to display 100% while still
// running. Asserts: the tail's running events have Total==0, Current==0, and
// progress falling in (0.95, 1.0); the done terminal state's
// Current/Total/Added semantics are unchanged.
func TestRunPipeline_TailClusteringDoesNotFillCurrentTotal(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()

	const n = 12
	var ids, paths []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("a%d", i)
		p := filepath.Join(dir, id+".jpg")
		require.NoError(t, os.WriteFile(p, []byte("img-"+id), 0o644))
		ids = append(ids, id)
		paths = append(paths, p)
	}
	for i := range ids {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, ids[i], paths[i])
		require.NoError(t, err)
	}

	ml := &pipelineMockML{}
	s := service.NewFaceService(db)
	s.SetML(ml)

	var emitted []service.Task
	var mu sync.Mutex
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)

	require.NoError(t, s.RunPipeline(context.Background()))

	mu.Lock()
	defer mu.Unlock()

	var sawTailRunning, sawDone bool
	for _, e := range emitted {
		if e.Type != "face" {
			continue
		}
		if e.Status == "running" && e.Progress > 0.95 && e.Progress < 1.0 {
			sawTailRunning = true
			require.Equal(t, int64(0), e.Total, "the clustering tail's running event Total should be 0, not equal to processed which would make the frontend show 100%% early")
			require.Equal(t, int64(0), e.Current, "the clustering tail's running event Current should be 0")
		}
		if e.Status == "done" {
			sawDone = true
			require.Equal(t, int64(n), e.Total, "the done event's Total semantics are unchanged: still the asset count awaiting detection")
			require.Equal(t, int64(n), e.Current, "the done event's Current semantics are unchanged: still the asset count awaiting detection")
		}
	}
	require.True(t, sawTailRunning, "should observe a running event from the clustering tail (progress in 0.95~1.0)")
	require.True(t, sawDone, "should have a face done event")
}

// TestRunPipeline_NothingToDoDoesNotPublish covers TDD case (2): when
// everything has been scanned and there are no unassigned faces, no task is
// published (taskReg has no face entry).
func TestRunPipeline_NothingToDoDoesNotPublish(t *testing.T) {
	db := makeTestFaceDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, face_scanned) VALUES('a1','/g/1.jpg','indexed',1)`)
	require.NoError(t, err)

	ml := &pipelineMockML{}
	s := service.NewFaceService(db)
	s.SetML(ml)

	var emitted []service.Task
	var mu sync.Mutex
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)

	require.NoError(t, s.RunPipeline(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, emitted, "should not publish any task when there's nothing to detect and no unassigned faces")
	require.Empty(t, ml.calls, "should not call ML")
}

// TestRunPipeline_SkipsUnreadableAssetWithoutInterrupting covers TDD case
// (3): a single asset's file read failure is skipped without interrupting
// the batch, and face_scanned stays 0.
func TestRunPipeline_SkipsUnreadableAssetWithoutInterrupting(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()

	// a1's source file doesn't exist (simulating a read failure); a2 is normal.
	missing := filepath.Join(dir, "missing.jpg")
	ok := filepath.Join(dir, "ok.jpg")
	require.NoError(t, os.WriteFile(ok, []byte("img-ok"), 0o644))

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1', ?, 'indexed'), ('a2', ?, 'indexed')`, missing, ok)
	require.NoError(t, err)

	ml := &pipelineMockML{}
	s := service.NewFaceService(db)
	s.SetML(ml)
	reg := service.NewTaskRegistry(func(service.Task) {})
	s.SetTaskRegistry(reg)

	require.NoError(t, s.RunPipeline(context.Background()))

	scanned := map[string]int{}
	rows, err := db.Query(`SELECT id, face_scanned FROM assets`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var fs int
		require.NoError(t, rows.Scan(&id, &fs))
		scanned[id] = fs
	}
	rows.Close()
	require.Equal(t, 0, scanned["a1"], "the asset that failed to read should keep face_scanned at 0, for the next retry")
	require.Equal(t, 1, scanned["a2"], "the normal asset should complete detection")

	var faceCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id='a1'`).Scan(&faceCount))
	require.Equal(t, 0, faceCount)
}

// TestRunPipeline_FacesDisabledReturnsImmediately covers TDD case (4):
// FacesEnabled=false returns immediately, with no query and no task published.
func TestRunPipeline_FacesDisabledReturnsImmediately(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: false}

	db := makeTestFaceDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
	require.NoError(t, err)

	ml := &pipelineMockML{}
	s := service.NewFaceService(db)
	s.SetML(ml)

	var emitted []service.Task
	var mu sync.Mutex
	reg := service.NewTaskRegistry(func(tk service.Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })
	s.SetTaskRegistry(reg)

	require.NoError(t, s.RunPipeline(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, emitted)
	require.Empty(t, ml.calls)

	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id='a1'`).Scan(&fs))
	require.Equal(t, 0, fs, "asset should not be detected while disabled")
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
	require.Greater(t, conf, 0.9, "a cluster of near-identical vectors should have high confidence")
	require.LessOrEqual(t, conf, 1.0)

	// A singleton cluster has confidence 1.0
	require.Equal(t, 1.0, service.ClusterConfidence([][]float32{normalize(v1)}, normalize(v1)))
	// Empty cluster is safe
	require.Equal(t, 0.0, service.ClusterConfidence(nil, nil))

	// A cluster of opposing vectors should have low confidence
	v3 := make([]float32, dim)
	v3[0] = -1.0
	vecs2 := [][]float32{normalize(v1), normalize(v3)}
	c2 := service.ComputeCentroid(vecs2)
	conf2 := service.ClusterConfidence(vecs2, c2)
	require.Less(t, conf2, 0.1, "a cluster of opposing vectors should have low confidence")

	// Mismatched dimensions return nil
	require.Nil(t, service.ComputeCentroid([][]float32{make([]float32, 4), make([]float32, 5)}))
}

// insertAssetFace is a test helper: writes one asset and one face embedding.
func insertAssetFace(t *testing.T, db *sql.DB, assetID string, vec []float32) string {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?, 'indexed')`,
		assetID, "/x/"+assetID+".jpg")
	require.NoError(t, err)
	faceID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
	return faceID
}

func TestRunClustering_HiddenPersonNotDeletedAndExcludedFromNewClusters(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "hp-1", normalize(a))
	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	// Get the sole person and mark it hidden=1
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)

	// Add a face close to the hidden one and rerun
	a2 := make([]float32, dim)
	a2[0] = 0.97
	a2[1] = 0.03
	insertAssetFace(t, db, "hp-2", normalize(a2))
	require.NoError(t, svc.RunClustering(context.Background()))

	// The hidden person is still there
	var hidden int
	require.NoError(t, db.QueryRow(`SELECT hidden FROM persons WHERE id=?`, pid).Scan(&hidden))
	require.Equal(t, 1, hidden)

	// The nearby face is snapped onto the hidden person (hidden also
	// participates in snapping as an anchor)
	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, pid).Scan(&cnt))
	require.Equal(t, 2, cnt)
}

func TestRunClustering_PreservesNamedAcrossReruns(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "asset-a1", normalize(a))

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	// Name the generated (sole) person
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, pid)
	require.NoError(t, err)

	// Add a new face close to Alice and rerun clustering
	a2 := make([]float32, dim)
	a2[0] = 0.97
	a2[1] = 0.03
	insertAssetFace(t, db, "asset-a2", normalize(a2))
	require.NoError(t, svc.RunClustering(context.Background()))

	// Alice is still there and still named Alice
	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM persons WHERE id=?`, pid).Scan(&name))
	require.Equal(t, "Alice", name)

	// The newly added face is snapped onto Alice (2 faces under Alice now)
	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, pid).Scan(&cnt))
	require.Equal(t, 2, cnt)

	// centroid/confidence have been written back
	var conf float64
	var centroid []byte
	require.NoError(t, db.QueryRow(`SELECT confidence, centroid FROM persons WHERE id=?`, pid).Scan(&conf, &centroid))
	require.Greater(t, conf, 0.0)
	require.NotEmpty(t, centroid)
}

// After deleting all photos (0 faces) and reclustering, orphan persons
// produced by auto-clustering should be cleaned up, not left behind by an
// early return.
func TestRunClustering_ZeroFaces_PurgesOrphanPersons(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "z-a1", normalize(a))
	insertAssetFace(t, db, "z-a2", normalize(a))
	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&before))
	require.Greater(t, before, 0, "should have persons after clustering")

	// Simulate "all photos deleted": clear out face_detections and assets.
	_, err := db.Exec(`DELETE FROM face_person`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM face_detections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM assets`)
	require.NoError(t, err)

	// Cluster again: the 0-face path should clean up orphan persons.
	require.NoError(t, svc.RunClustering(context.Background()))

	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&after))
	require.Equal(t, 0, after, "auto orphan persons should be cleared when there are 0 faces")
}

// The 0-face cleanup only deletes non-anchored persons; user-named persons should be kept.
func TestRunClustering_ZeroFaces_KeepsNamedPerson(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "k-a1", normalize(a))
	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	// Name the sole person (anchoring it).
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, pid)
	require.NoError(t, err)

	// Delete all photos/faces and recluster.
	_, err = db.Exec(`DELETE FROM face_person`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM face_detections`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM assets`)
	require.NoError(t, err)
	require.NoError(t, svc.RunClustering(context.Background()))

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE name='Alice'`).Scan(&cnt))
	require.Equal(t, 1, cnt, "the named (anchored) person should be kept")
}
