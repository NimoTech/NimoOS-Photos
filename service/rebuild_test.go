package service

import (
	"context"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// TestRebuildReprocessesAssetsWithoutDuplicatingFaces 验证重建重算了 ML、
// 不会让 face_detections 翻倍，并写入 photos_meta 时间戳。
func TestRebuildReprocessesAssetsWithoutDuplicatingFaces(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())
	require.True(t, ix.processFileInternal(path, processOpts{}))

	clipBefore := ml.clipCalls
	faces := NewFaceService(db)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 2)

	taskID, err := rb.Start()
	require.NoError(t, err)
	require.NotEmpty(t, taskID)

	// 等待后台任务完成（running 标志复位）。
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	require.Greater(t, ml.clipCalls, clipBefore) // 确实重算了

	var faceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections`).Scan(&faceRows))
	require.LessOrEqual(t, faceRows, 1) // mockML 返回 0 张脸 → 重建后不残留旧行

	var lastBuilt string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='index_last_rebuilt'`).Scan(&lastBuilt))
	require.NotEmpty(t, lastBuilt)
}

// TestModelGenStale 验证 modelGenStale 对代次键缺失/匹配/落后三种情况的判断。
func TestModelGenStale(t *testing.T) {
	db := makeTestDB(t)
	if !modelGenStale(db) {
		t.Fatal("fresh db should be stale (no ml_model_gen key)")
	}
	if _, err := db.Exec(`INSERT INTO photos_meta(key,value) VALUES('ml_model_gen',?)`, common.MLModelGen); err != nil {
		t.Fatal(err)
	}
	if modelGenStale(db) {
		t.Fatal("db with current gen should not be stale")
	}
	if _, err := db.Exec(`UPDATE photos_meta SET value='1' WHERE key='ml_model_gen'`); err != nil {
		t.Fatal(err)
	}
	if !modelGenStale(db) {
		t.Fatal("db with old gen should be stale")
	}
}

// TestRebuildRejectsConcurrentRuns 验证重复触发返回 ErrRebuildRunning。
func TestRebuildRejectsConcurrentRuns(t *testing.T) {
	db := makeTestDB(t)
	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	rb.running.Store(true) // 模拟运行中
	_, err := rb.Start()
	require.ErrorIs(t, err, ErrRebuildRunning)
}

// TestRebuildEmptyLibraryFinishesImmediately 空库重建直接完成且写入 meta。
func TestRebuildEmptyLibraryFinishesImmediately(t *testing.T) {
	db := makeTestDB(t)
	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	_, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)
	var lastBuilt string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='index_last_rebuilt'`).Scan(&lastBuilt))
	require.NotEmpty(t, lastBuilt)
}
