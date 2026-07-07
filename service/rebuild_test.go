package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// seedFaceAndClip 给已存在的 asset 写入一行 face_detections 和一条 CLIP 向量，
// 用于验证 rebuild 对“源文件不可读”的资产是否保留旧 ML 数据。
func seedFaceAndClip(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		"face-"+assetID, assetID, "[0,0,1,1]", sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid))
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(make([]float32, common.CLIPDim)))
	require.NoError(t, err)
}

// TestRebuildKeepsExistingMLDataForUnreadableSource 验证:当资产的源文件在重建时
// 不可读(例如放在已拔出的移动盘上), rebuild 绝不能删除它现有的 face_detections /
// CLIP 向量——processFileInternal 会在第一步 os.ReadFile 就失败并返回，之前若已经
// 删了旧数据，就永久静默丢失、无法恢复。可正常读取的资产应照常被重新处理。
func TestRebuildKeepsExistingMLDataForUnreadableSource(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	dir := t.TempDir()
	okPath := makeTestJPEG(t, dir)
	missingPath := filepath.Join(dir, "gone-missing.jpg") // 从不创建：模拟不可读源

	const okID = "asset-ok"
	const missingID = "asset-missing"
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, okID, okPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, missingID, missingPath)
	require.NoError(t, err)

	seedFaceAndClip(t, db, okID)
	seedFaceAndClip(t, db, missingID)

	var missingRowidBefore int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, missingID).Scan(&missingRowidBefore))

	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	faces := NewFaceService(db)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 2)

	taskID, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	// 不可读资产：旧 face_detections 行必须原样保留。
	var faceCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, missingID).Scan(&faceCount))
	require.Equal(t, 1, faceCount, "源文件不可读时必须保留旧的人脸行")

	// 不可读资产：旧 CLIP 向量（同一 rowid）必须原样保留。
	var missingRowidAfter int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, missingID).Scan(&missingRowidAfter))
	require.Equal(t, missingRowidBefore, missingRowidAfter)
	var vecCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_embeddings WHERE rowid=?`, missingRowidAfter).Scan(&vecCount))
	require.Equal(t, 1, vecCount, "源文件不可读时必须保留旧的 CLIP 向量")

	// 可读资产：正常重跑（mockML 记录了 CLIP 调用；重跑后 0 张脸，旧行已被替换）。
	require.Greater(t, ml.clipCalls, 0)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, okID).Scan(&faceCount))
	require.Equal(t, 0, faceCount, "可读资产应重新处理，旧人脸行被清空后由 mockML(0 张脸)的结果替代")

	// 任务 label 计入 1 张失败。
	var label string
	for _, task := range reg.List() {
		if task.ID == taskID {
			label = task.Label
		}
	}
	require.Contains(t, label, "失败 1 张")
}

// TestRebuildPrunesOrphanClipVectors 验证:去掉全库清空之后，孤儿 CLIP 向量
// （asset 已不存在的 asset_clip_idx / clip_embeddings 行）仍会被 rebuild 的
// finalize() 阶段通过 pruneOrphanClipVectors 清理掉。
func TestRebuildPrunesOrphanClipVectors(t *testing.T) {
	db := makeTestDB(t)
	// asset_clip_idx.asset_id 有 FK(ON DELETE CASCADE)，正常路径下孤儿只会由
	// “绕过级联的历史删除”产生（参考 clipvec_internal_test.go）；这里同样临时关闭
	// FK 约束来模拟这种遗留孤儿状态，而不是真的破坏级联本身。
	const rowid = 888888
	_, err := db.Exec(`PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(rowid, asset_id) VALUES(?,?)`, rowid, "ghost")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(make([]float32, common.CLIPDim)))
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA foreign_keys=ON`)
	require.NoError(t, err)

	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	_, err = rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, "ghost").Scan(&n))
	require.Equal(t, 0, n, "孤儿 asset_clip_idx 行必须被清理")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_embeddings WHERE rowid=?`, rowid).Scan(&n))
	require.Equal(t, 0, n, "孤儿 CLIP 向量必须被清理")
}

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

// TestRebuildRedetectsFacesViaRunPipeline 验证:人脸检测移出 processFileInternal
// 后，rebuild 的 finalize() 必须改调 RunPipeline（而非只重新分组的 RunClustering）
// 才能把 worker 循环里删掉的旧脸重新检测回来——worker 循环删 face_detections 时
// 已把对应资产的 face_scanned 置回 0，若 finalize 仍调 RunClustering，这批脸会
// 永久清空、再也不会被检测。
func TestRebuildRedetectsFacesViaRunPipeline(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	thumbDir := t.TempDir()
	path := makeTestJPEG(t, t.TempDir())

	ml := &oneFaceML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))

	// 人脸检测已移出索引流水线：processFileInternal 之后不应有 face_detections。
	var faceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&faceRows))
	require.Zero(t, faceRows, "人脸检测已移出索引流水线，processFileInternal 不应再写 face_detections")

	// 手工模拟"已经跑过一轮 RunPipeline"：写入一行旧脸 + face_scanned=1。
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		"old-face", assetID, "[0,0,1,1]", sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	faces := NewFaceService(db)
	faces.SetML(ml)
	faces.SetThumbDir(thumbDir)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 1)

	_, err = rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	// finalize() 里的 faces.RunPipeline 在 run() 的同一 goroutine 内同步执行，
	// running 复位时它已经跑完。
	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 1, fs, "重建后应重新检测完成，face_scanned 应回到 1")

	var newFaceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&newFaceRows))
	require.Equal(t, 1, newFaceRows, "旧脸被删后应由 RunPipeline 重新检测出 1 张新脸，而不是永久清空")
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

// TestRebuildExcludesOfflineAssets 验证:资产所在的移动盘已拔出(offline=1)
// 时,rebuild 的目标查询必须跳过它——它的源文件读不到,处理只会白白计一次失败;
// MountGuard 会在插回时主动触发一次 Backfill/BackfillOCR 来补齐期间的缺口。
func TestRebuildExcludesOfflineAssets(t *testing.T) {
	db := makeTestDB(t)
	onlineID := insertAsset(t, db, "/DATA/Gallery/online.jpg", "indexed")
	offlineID := insertAsset(t, db, "/media/X/offline.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offlineID)
	require.NoError(t, err)

	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	taskID, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)

	var total int64 = -1
	for _, task := range rb.reg.List() {
		if task.ID == taskID {
			total = task.Total
		}
	}
	require.Equal(t, int64(1), total, "offline 资产不应计入 rebuild 目标")
	_ = onlineID
}

// TestShouldStampModelGen 验证:本轮一个文件都没真正用新模型重跑成功
// (total>0 且全失败)时不得盖章 ml_model_gen，否则 modelGenStale 判定永远不再
// 触发，MaybeAutoRebuild 失去自动重试机会（典型场景：模型换代恰逢移动盘整体离线）。
func TestShouldStampModelGen(t *testing.T) {
	require.True(t, shouldStampModelGen(10, 0))   // 全成功
	require.True(t, shouldStampModelGen(10, 3))   // 部分失败:照常盖章
	require.True(t, shouldStampModelGen(0, 0))    // 空库:盖章合法
	require.False(t, shouldStampModelGen(10, 10)) // 全失败:不盖章
}
