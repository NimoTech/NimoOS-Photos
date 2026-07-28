package service

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// mockML implements MLProvider — all methods return zero vectors / nil errors.
type mockML struct{}

func (m *mockML) CLIPImageEmbed(_ []byte) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *mockML) CLIPTextEmbed(_ string) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *mockML) DetectAndRecognizeFaces(_ []byte) ([]mlclient.FaceResult, error) {
	return nil, nil
}
func (m *mockML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{}, nil
}
func (m *mockML) IsReady() bool { return true }

// makeTestDB opens a fresh in-memory SQLite database (schema migrated).
func makeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(tmp)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// makeTestJPEG writes a minimal valid JPEG to dir and returns its path.
func makeTestJPEG(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	path := filepath.Join(dir, "test.jpg")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return path
}

// oneFaceML 总是报告检测到恰好一张脸——用于区分"ML 没被调用"（因为压根没
// 调）和"ML 被调用但结果没被持久化"，比返回 0 张脸的 mockML/recordingML 更能
// 证明人脸检测确实已经移出索引流水线（而不是恰好每次都测不出脸）。
type oneFaceML struct{ mockML }

func (m *oneFaceML) DetectAndRecognizeFaces(_ []byte) ([]mlclient.FaceResult, error) {
	vec := make([]float32, common.FaceDim)
	vec[0] = 1
	return []mlclient.FaceResult{{BBox: mlclient.BoundingBox{X1: 0, Y1: 0, X2: 1, Y2: 1}, Embedding: vec}}, nil
}

// writeJPEGAt 把一张纯色 JPEG 写到指定路径，颜色由 seed 决定，用于制造"同一
// file_path、不同内容(不同 checksum)"的场景。
func writeJPEGAt(t *testing.T, path string, seed int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	c := color.RGBA{R: uint8(seed * 37 % 256), G: uint8(seed * 53 % 256), B: uint8(seed * 97 % 256), A: 255}
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
}

// TestIndexerProcessFile_DoesNotDetectFaces 断言人脸检测已移出索引流水线：
// processFileInternal 之后 face_detections 为空、face_scanned=0，即便 ML 会
// 返回真实的人脸结果也不会被调用/写入——检测交给独立的 FaceService.RunPipeline
// （0→95% 真实进度 + 95→100% 聚类尾段）。
func TestIndexerProcessFile_DoesNotDetectFaces(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	ml := &oneFaceML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))

	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))

	var faceCount, faceScanned int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&faceCount))
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&faceScanned))
	require.Zero(t, faceCount, "人脸检测已移出索引流水线，不应再写 face_detections")
	require.Zero(t, faceScanned, "face_scanned 应保持 0，等待 RunPipeline 处理")
}

// TestReprocess_ContentChange_ResetsFaceScanned 断言:同一 file_path 内容真的
// 变了(checksum 变化)时,重新处理会把 face_scanned 置回 0，交给 RunPipeline
// 重新检测——覆盖"编辑/替换了原图但路径不变"的场景。
func TestReprocess_ContentChange_ResetsFaceScanned(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")
	writeJPEGAt(t, path, 1)

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// 内容真的变了：换一张不同的图片写到同一路径（checksum 必然不同）。
	writeJPEGAt(t, path, 2)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 0, fs, "内容变化(checksum 不同)应把 face_scanned 置回 0")
}

// TestForceReprocess_UnchangedContent_PreservesFaceScanned 断言:force
// 重跑但文件内容(checksum)没变时（如 Embedder/Rebuilder 的纯 CLIP 补跑），
// face_scanned 不应被清掉——否则每轮 CLIP 补跑都会把同一批资产重新扔回
// 人脸检测队列，在 face_detections 里产生重复行。
func TestForceReprocess_UnchangedContent_PreservesFaceScanned(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// 同一份文件内容，force=true 只是绕过"已 indexed 跳过"短路（照 Embedder
	// 的 ForceReprocess(processOpts{force:true, skipExif:true, skipThumb:true})用法）。
	ok := ix.ForceReprocess(path, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 1, fs, "内容未变时 force 重跑不应清掉 face_scanned")
}

// TestReprocess_ContentChange_ResetsCaptionSynced 断言:同一 file_path 内容真的
// 变了(checksum 变化)时,重新处理会把 caption_synced 置回 0，交给照片知识库
// 投喂管线重新交接给 Parser——覆盖"编辑/替换了原图但路径不变"的场景。
func TestReprocess_ContentChange_ResetsCaptionSynced(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")
	writeJPEGAt(t, path, 1)

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET caption_synced=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// 内容真的变了：换一张不同的图片写到同一路径（checksum 必然不同）。
	writeJPEGAt(t, path, 2)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var cs int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&cs))
	require.Equal(t, 0, cs, "内容变化(checksum 不同)应把 caption_synced 置回 0")
}

// TestForceReprocess_UnchangedContent_PreservesCaptionSynced 断言:force
// 重跑但文件内容(checksum)没变时（如 Embedder/Rebuilder 的纯 CLIP 补跑），
// caption_synced 不应被清掉——否则每轮补跑都会把同一批已交接 Parser 的资产
// 重新扔回投喂队列，产生重复投喂。
func TestForceReprocess_UnchangedContent_PreservesCaptionSynced(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET caption_synced=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// 同一份文件内容，force=true 只是绕过"已 indexed 跳过"短路（照 Embedder
	// 的 ForceReprocess(processOpts{force:true, skipExif:true, skipThumb:true})用法）。
	ok := ix.ForceReprocess(path, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	var cs int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&cs))
	require.Equal(t, 1, cs, "内容未变时 force 重跑不应清掉 caption_synced")
}

// boxedML 返回带文字框的 OCR 结果，用于覆盖率计算测试。
type boxedML struct{ mockML }

func (m *boxedML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{
		// 0.4 宽 × 0.1 高 = 4% 面积
		{Text: "TOTAL $42.00", Score: 0.97, Box: []float64{0.1, 0.1, 0.5, 0.1, 0.5, 0.2, 0.1, 0.2}},
		// 0.2 × 0.05 = 1% 面积
		{Text: "Invoice #1", Score: 0.95, Box: []float64{0.1, 0.3, 0.3, 0.3, 0.3, 0.35, 0.1, 0.35}},
		// 低置信度行：不计入文本、行数和覆盖率
		{Text: "noise", Score: 0.2, Box: []float64{0, 0, 1, 0, 1, 1, 0, 1}},
	}, nil
}

// TestOcrAssetStoresCoverageAndLines 验证 ocrAsset 把覆盖率（文字框面积合计）
// 和保留行数一起入库，且低置信度行被过滤。
func TestOcrAssetStoresCoverageAndLines(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &boxedML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var text string
	var coverage float64
	var lines int
	require.NoError(t, db.QueryRow(
		`SELECT text, coverage, line_count FROM asset_ocr WHERE asset_id='a1'`,
	).Scan(&text, &coverage, &lines))
	require.Equal(t, "TOTAL $42.00\nInvoice #1", text)
	require.Equal(t, 2, lines)
	require.InDelta(t, 0.05, coverage, 1e-9) // 4% + 1%
}

// TestOcrAssetStoresLineBoxes 验证 ocrAsset 把保留行的文本+坐标+置信度逐行
// 写入 asset_ocr_lines(低置信度行不写),boxes_ver 置 1;重跑覆盖旧行。
func TestOcrAssetStoresLineBoxes(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &boxedML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var ver int
	require.NoError(t, db.QueryRow(`SELECT boxes_ver FROM asset_ocr WHERE asset_id='a1'`).Scan(&ver))
	require.Equal(t, 1, ver)

	type row struct {
		text, box string
		score     float64
	}
	readLines := func() []row {
		rows, err := db.Query(`SELECT text, box, score FROM asset_ocr_lines WHERE asset_id='a1' ORDER BY line_no`)
		require.NoError(t, err)
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.text, &r.box, &r.score))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}

	got := readLines()
	require.Len(t, got, 2, "低置信度行(noise)不应入库")
	require.Equal(t, "TOTAL $42.00", got[0].text)
	require.Equal(t, "[0.1,0.1,0.5,0.1,0.5,0.2,0.1,0.2]", got[0].box)
	require.InDelta(t, 0.97, got[0].score, 1e-9)
	require.Equal(t, "Invoice #1", got[1].text)

	// 重跑覆盖:第二次结果替换第一次,不残留旧行。
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))
	require.Len(t, readLines(), 2)

	// 重 OCR 必须重置 doc_ver,让上层重新算 doc 判定;is_doc 保留旧值,
	// 重算前查询平滑沿用(见 hasOcrExpr NULL 回退,此处非 NULL 故仍看旧值)。
	_, err = db.Exec(`UPDATE asset_ocr SET doc_ver=1, is_doc=1 WHERE asset_id='a1'`)
	require.NoError(t, err)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))
	var docVer, isDoc int
	require.NoError(t, db.QueryRow(`SELECT doc_ver, is_doc FROM asset_ocr WHERE asset_id='a1'`).Scan(&docVer, &isDoc))
	require.Equal(t, 0, docVer, "重 OCR 必须重置 doc_ver 触发重算")
	require.Equal(t, 1, isDoc, "is_doc 保留旧值,重算前平滑沿用")
}

// TestOcrAssetNilBoxStored 验证 ML 未返回几何(Box=nil)时行仍入库,box 存 '[]'。
type nilBoxML struct{ mockML }

func (m *nilBoxML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{{Text: "no geometry", Score: 0.9, Box: nil}}, nil
}

func TestOcrAssetNilBoxStored(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &nilBoxML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var box string
	require.NoError(t, db.QueryRow(`SELECT box FROM asset_ocr_lines WHERE asset_id='a1' AND line_no=0`).Scan(&box))
	require.Equal(t, "[]", box)
}

// TestIndexerProcessesImage tests the full pipeline for a single image.
func TestIndexerProcessesImage(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)

	idx.Enqueue(imgPath)

	// Wait up to 4s for the asset to reach "indexed" status.
	var assetID string
	require.Eventually(t, func() bool {
		var status string
		err := db.QueryRow(
			`SELECT id, status FROM assets WHERE file_path=?`, imgPath,
		).Scan(&assetID, &status)
		return err == nil && status == "indexed"
	}, 4*time.Second, 100*time.Millisecond, "asset should reach 'indexed' status")

	require.NotEmpty(t, assetID, "asset ID must be populated")

	// Verify thumbnail exists.
	smallPath := filepath.Join(thumbDir, assetID, "small.jpg")
	_, err := os.Stat(smallPath)
	require.NoError(t, err, "small.jpg thumbnail must exist at %s", smallPath)
}

// TestIndexerDeduplicates enqueues the same path twice and verifies only one
// DB record is created.
func TestIndexerDeduplicates(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 2)
	go idx.Start(ctx)

	// Enqueue twice — the second should be a no-op.
	idx.Enqueue(imgPath)
	idx.Enqueue(imgPath)

	// Wait for indexed.
	require.Eventually(t, func() bool {
		var status string
		err := db.QueryRow(
			`SELECT status FROM assets WHERE file_path=?`, imgPath,
		).Scan(&status)
		return err == nil && status == "indexed"
	}, 4*time.Second, 100*time.Millisecond, "asset should reach 'indexed' status")

	// Exactly one record.
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM assets WHERE file_path=?`, imgPath,
	).Scan(&count))
	require.Equal(t, 1, count, "duplicate enqueue must not create duplicate DB rows")
}

// TestIndexer_ForceReprocess_BypassesChecksumShortcut 断言 ForceReprocess
// 在 asset 已经 indexed 时仍能再跑一次 ML，写入缺失的 clip_embeddings。
func TestIndexer_ForceReprocess_BypassesChecksumShortcut(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 第一遍：用一个 ML "不就绪" 的 mock 跑索引，模拟"asset 被标 indexed 但没向量"的真实场景。
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	var hasIdx int
	_ = db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&hasIdx)
	require.Equal(t, 0, hasIdx, "precondition: asset 应该没有 clip embedding")

	// 第二遍：替换 ML 为 ready 的 mock，调 ForceReprocess。
	cancel() // 关掉旧 worker
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ready := &mockML{}
	idx2 := NewIndexer(db, ready, thumbDir, 1)
	go idx2.Start(ctx2)

	ok := idx2.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok, "ForceReprocess 应返回 true")

	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&n)
		return n == 1
	}, 2*time.Second, 50*time.Millisecond)
}

// mockMLNotReady 永远返回 IsReady=false，强制 processFile 跳过 ML 段。
type mockMLNotReady struct{ mockML }

func (m *mockMLNotReady) IsReady() bool { return false }

// TestIndexer_ForceReprocess_SkipExif 断言 skipExif=true 时不动 asset_exif 行。
func TestIndexer_ForceReprocess_SkipExif(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	// 故意污染 asset_exif: 写一个明显错误的值
	_, err := db.Exec(`UPDATE asset_exif SET width=99999 WHERE asset_id=?`, assetID)
	require.NoError(t, err)

	ok := idx.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	// skipExif 生效时，污染值仍然保留
	var w int
	require.NoError(t, db.QueryRow(`SELECT width FROM asset_exif WHERE asset_id=?`, assetID).Scan(&w))
	require.Equal(t, 99999, w, "skipExif=true 时应保留原 asset_exif")
}

// TestIndexer_ForceReprocess_SkipThumb 断言 skipThumb=true 时不重生缩略图。
func TestIndexer_ForceReprocess_SkipThumb(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	// 替换缩略图目录里某个文件为 sentinel（取目录下任一已存在的图）
	entries, err := os.ReadDir(filepath.Join(thumbDir, assetID))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "precondition: 应已生成缩略图")
	sentinel := filepath.Join(thumbDir, assetID, entries[0].Name())
	require.NoError(t, os.WriteFile(sentinel, []byte{0}, 0644))

	ok := idx.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	info, err := os.Stat(sentinel)
	require.NoError(t, err)
	require.Equal(t, int64(1), info.Size(), "skipThumb=true 时应保留原缩略图（sentinel 仍是 1 字节）")
}

// TestAssetExifUpsertReplacesOnConflict drives the asset_exif upsert SQL
// directly to confirm that ON CONFLICT(asset_id) DO UPDATE replaces only the
// columns listed in the DO UPDATE clause, leaving previously-written columns
// untouched. This guards the indexer's image→video sequential write path.
func TestAssetExifUpsertReplacesOnConflict(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Seed an asset row so the FK is satisfied.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/tmp/a.jpg','pending')`)
	require.NoError(t, err)

	// First write (image-style: with iso).
	_, err = db.Exec(`
		INSERT INTO asset_exif(asset_id, width, height, iso, aperture, make)
		VALUES('a1', 100, 200, 800, 1.8, 'Apple')
		ON CONFLICT(asset_id) DO UPDATE SET
		  width = excluded.width,
		  height = excluded.height,
		  iso = excluded.iso,
		  aperture = excluded.aperture,
		  make = excluded.make`)
	require.NoError(t, err)

	var width, iso int
	var aperture float64
	var make string
	require.NoError(t, db.QueryRow(
		`SELECT width, iso, aperture, make FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&width, &iso, &aperture, &make))
	require.Equal(t, 100, width)
	require.Equal(t, 800, iso)
	require.InDelta(t, 1.8, aperture, 1e-6)
	require.Equal(t, "Apple", make)

	// Second write (video-style: different columns; conflicts on asset_id).
	_, err = db.Exec(`
		INSERT INTO asset_exif(asset_id, width, height, video_codec, frame_rate, bit_rate, rotation)
		VALUES('a1', 1920, 1080, 'h264', 29.97, 12000000, 90)
		ON CONFLICT(asset_id) DO UPDATE SET
		  width = excluded.width,
		  height = excluded.height,
		  video_codec = excluded.video_codec,
		  frame_rate = excluded.frame_rate,
		  bit_rate = excluded.bit_rate,
		  rotation = excluded.rotation`)
	require.NoError(t, err)

	var w2, h2, br, rot int
	var codec string
	var fps float64
	require.NoError(t, db.QueryRow(
		`SELECT width, height, video_codec, frame_rate, bit_rate, rotation FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&w2, &h2, &codec, &fps, &br, &rot))
	require.Equal(t, 1920, w2)
	require.Equal(t, 1080, h2)
	require.Equal(t, "h264", codec)
	require.InDelta(t, 29.97, fps, 1e-3)
	require.Equal(t, 12000000, br)
	require.Equal(t, 90, rot)

	// Image-side columns from the first write should still be there (they were
	// NOT listed in the second upsert's DO UPDATE clause).
	var oldIso int
	var oldAp float64
	var oldMake string
	require.NoError(t, db.QueryRow(
		`SELECT iso, aperture, make FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&oldIso, &oldAp, &oldMake))
	require.Equal(t, 800, oldIso)
	require.InDelta(t, 1.8, oldAp, 1e-6)
	require.Equal(t, "Apple", oldMake)
}

func TestScanDirectorySkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(root, ".trash", "id1")
	if err := os.MkdirAll(trashDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trashDir, "b.jpg"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	var collected []string
	err := walkSupported(context.Background(), root, func(p string) { collected = append(collected, p) })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range collected {
		if strings.Contains(p, ".trash") {
			t.Fatalf("scan should skip .trash, but collected %q", p)
		}
	}
	if len(collected) != 1 {
		t.Fatalf("collected %d files, want 1", len(collected))
	}
}

// TestRemoveByPathSkipsTrashedAsset is a regression test for the watcher race:
// soft-deleting moves the file, which fires an fsnotify Rename on the old path;
// the watcher calls RemoveByPath, which must NOT hard-delete a trashed asset.
func TestRemoveByPathSkipsTrashedAsset(t *testing.T) {
	db := makeTestDB(t)
	idx := NewIndexer(db, &mockML{}, t.TempDir(), 1)

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, deleted_at, original_path)
		VALUES('t1', '/DATA/Gallery/foo.jpg', 'indexed', CURRENT_TIMESTAMP, '/DATA/Gallery/foo.jpg')`)
	require.NoError(t, err)

	idx.RemoveByPath("/DATA/Gallery/foo.jpg")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id='t1'`).Scan(&n))
	require.Equal(t, 1, n, "RemoveByPath must not delete a soft-deleted asset")
}

// TestScanDirectoryOnceDedups:同一根目录的并发补扫只跑一份——挂载轮询
// (watcher followMounts)与 MountGuard 插回恢复都可能对同一挂载触发补扫，
// 重复扫描徒耗 IO。in-flight 标记被占用时直接跳过，释放后可再次扫描。
func TestScanDirectoryOnceDedups(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()

	ix.scanDirInFlight.Store(dir, struct{}{}) // 模拟另一路补扫在跑
	started, err := ix.ScanDirectoryOnce(dir)
	require.NoError(t, err)
	require.False(t, started, "in-flight 时必须跳过")

	ix.scanDirInFlight.Delete(dir)
	started, err = ix.ScanDirectoryOnce(dir)
	require.NoError(t, err)
	require.True(t, started, "释放后必须真正执行")
}

// recordingML 记录各 ML 能力被调用的次数，用于开关门控断言。
// lastOCRData 额外记录最近一次 OCR 调用收到的字节，用于断言超大原图场景下
// OCR 收到的是降级缩略图而不是原图。
type recordingML struct {
	mockML
	clipCalls, faceCalls, ocrCalls int
	lastOCRData                    []byte
}

func (m *recordingML) CLIPImageEmbed(d []byte) ([]float32, error) {
	m.clipCalls++
	return m.mockML.CLIPImageEmbed(d)
}
func (m *recordingML) DetectAndRecognizeFaces(d []byte) ([]mlclient.FaceResult, error) {
	m.faceCalls++
	return m.mockML.DetectAndRecognizeFaces(d)
}
func (m *recordingML) OCR(d []byte) ([]mlclient.OCRLine, error) {
	m.ocrCalls++
	m.lastOCRData = d
	return m.mockML.OCR(d)
}

// TestIndexerHonorsFeatureFlags 验证 Scenes/OCR/Faces 关闭时索引器跳过对应 ML 调用。
func TestIndexerHonorsFeatureFlags(t *testing.T) {
	db := makeTestDB(t)
	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })

	config.Cfg = &config.Config{FacesEnabled: false, ScenesEnabled: false, OCREnabled: false}
	require.True(t, ix.processFileInternal(path, processOpts{force: true}))
	require.Zero(t, ml.clipCalls)
	require.Zero(t, ml.ocrCalls)

	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}
	require.True(t, ix.processFileInternal(path, processOpts{force: true}))
	require.Equal(t, 1, ml.clipCalls)
	require.Equal(t, 1, ml.ocrCalls)

	// 人脸检测已移出索引流水线，交给独立的 FaceService.RunPipeline（真实进度
	// 任务）：无论 FacesEnabled 取值如何，processFileInternal 都不应再直接调用
	// DetectAndRecognizeFaces。
	require.Zero(t, ml.faceCalls, "人脸检测已移交 RunPipeline，索引器不应再直接调用 ML")
}

// TestProcessFileInternal_OCRFallsBackToThumbnailForOversizedOriginal 覆盖真实
// 定位到的 bug：原图超过 immich-ml/PIL 178.9MP 硬上限（真实案例是库里
// 16320x12240=199.8MP 的 Pexels 照片）时，内联 OCR 必须改用已生成的 large.jpg
// 缩略图代替原图字节，否则 OCR 请求必然 500 而被永久吞掉。
//
// 用一条已知 id 的预置记录让 processFileInternal 走 ON CONFLICT(file_path)
// UPDATE 分支（不改动已有 id），从而能在调用前就把 large.jpg 缩略图放到
// 已知路径下——真实的 thumb.Generate 对这段手工构造的 JPEG 头(无真实像素
// 数据)必然解码失败，不会覆盖预置的缩略图。
func TestProcessFileInternal_OCRFallsBackToThumbnailForOversizedOriginal(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	const assetID = "asset-ocr-oversized"

	oversizedPath := filepath.Join(imgDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'pending')`, assetID, oversizedPath)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, assetID), 0o755))
	generatedThumb := makeTestJPEG(t, t.TempDir())
	thumbBytes, err := os.ReadFile(generatedThumb)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, assetID, "large.jpg"), thumbBytes, 0o644))

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(oversizedPath, processOpts{force: true}))

	require.Equal(t, 1, ml.ocrCalls)
	require.Equal(t, thumbBytes, ml.lastOCRData, "OCR 应收到 large.jpg 缩略图字节而不是超限原图字节")
}

// TestProcessFileInternal_OCRSkippedWhenOversizedAndNoThumbnail 覆盖降级也拿
// 不到缩略图的情况：真实的 thumb.Generate 对这段无有效像素数据的手工 JPEG 头
// 必然解码失败、不产出 large.jpg/small.jpg，此时 OCR 必须被跳过（沿用既有的
// 吞错风格），而不是把超限原图硬塞给 ML。
func TestProcessFileInternal_OCRSkippedWhenOversizedAndNoThumbnail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	oversizedPath := filepath.Join(imgDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(oversizedPath, processOpts{force: true}))

	require.Zero(t, ml.ocrCalls, "缩略图不可用时应跳过 OCR，而不是把超限原图传给 ML")
}

// TestResolveMimeType 验证我们为支持的媒体扩展名存储权威的 MIME 类型，而不是
// http.DetectContentType 的内容嗅探结果——后者对 QuickTime(.mov) 与 HEIC 返回
// application/octet-stream，对 Matroska(.mkv) 返回误导性的 video/webm。整个系统
// （前端用 mime_type 选择 <video>/<img>，后端所有 "mime_type LIKE 'video/%'"
// 查询）都依赖该字段，因此必须存对。
func TestResolveMimeType(t *testing.T) {
	cases := []struct {
		ext  string
		want string
	}{
		{".mov", "video/quicktime"},
		{".mkv", "video/x-matroska"},
		{".avi", "video/x-msvideo"},
		{".mp4", "video/mp4"},
		{".heic", "image/heic"},
		{".webp", "image/webp"},
		{".jpg", "image/jpeg"},
		{".jpeg", "image/jpeg"},
		{".png", "image/png"},
		{".MOV", "video/quicktime"}, // 大小写不敏感
		// 扩充的图片格式
		{".gif", "image/gif"},
		{".bmp", "image/bmp"},
		{".tiff", "image/tiff"},
		{".tif", "image/tiff"},
		{".avif", "image/avif"},
		// 扩充的视频格式（仅浏览器可原生内联播放的）
		{".webm", "video/webm"},
		{".m4v", "video/mp4"},
		{".3gp", "video/3gpp"},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, resolveMimeType(nil, c.ext),
			"resolveMimeType(%q) 应返回权威类型", c.ext)
	}

	// 未知扩展名回退到内容嗅探。
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	require.Equal(t, "image/png", resolveMimeType(pngHeader, ".bin"),
		"未知扩展名应回退到 http.DetectContentType")
}

// TestPruneMissingUnderSkipsWhenMountVanished:恢复扫描(插回 → ScanDirectory)
// 末尾的 prune 与「扫描期间盘又被拔出、挂载点残留空目录」相撞时,该目录下所有
// 文件都会 stat 成"不存在"——若照常 prune,整块盘的资产连行带向量、缩略图会被
// 物理删光。互锁 ①:目录对应挂载点不在当前挂载表时必须跳过 prune。
func TestPruneMissingUnderSkipsWhenMountVanished(t *testing.T) {
	db := makeTestDB(t)
	// 夹具必须用真实机器上不存在的挂载名(见 TestPruneMissingUnderKeepsOfflineAssets
	// 的说明):/media/devmon 在本机是 0700,stat 报 EACCES 而非 ENOENT,会把
	// 本用例架空(资产无论互锁存在与否都会保留,断言恒真)。
	id := insertAsset(t, db, "/media/nimoos-test-V/gone.jpg", "indexed") // 文件在磁盘上不存在
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA"} } // /media/nimoos-test-V 已从挂载表消失

	require.NoError(t, ix.pruneMissingUnder("/media/nimoos-test-V"))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, id).Scan(&n))
	require.Equal(t, 1, n, "挂载点已消失时禁止 prune,资产必须原样保留")
}

// TestPruneMissingUnderKeepsOfflineAssets:互锁 ②——offline=1 资产的文件读不到
// 恰恰是 offline 标记本身记录的状态,不是"文件被删"的证据,prune 必须排除它们;
// offline=0 且文件确实消失的资产照常清理。
func TestPruneMissingUnderKeepsOfflineAssets(t *testing.T) {
	db := makeTestDB(t)
	// 挂载点用真实存在的空临时目录:pruneDeleteAllowed 在删除前会 os.Stat 所属
	// 挂载根复核其仍健在(见 pruneDeleteAllowed),用不存在的虚构路径会让该复核
	// 恒假、架空本用例;目录下的具体文件仍不创建,以保留"文件消失"语义。
	mountDir := t.TempDir()
	offID := insertAsset(t, db, filepath.Join(mountDir, "offline.jpg"), "indexed")
	onID := insertAsset(t, db, filepath.Join(mountDir, "deleted.jpg"), "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offID)
	require.NoError(t, err)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA", mountDir} }

	require.NoError(t, ix.pruneMissingUnder(mountDir))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, offID).Scan(&n))
	require.Equal(t, 1, n, "offline=1 资产必须被 prune 排除")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, onID).Scan(&n))
	require.Equal(t, 0, n, "offline=0 且文件确实消失的资产应照常清理")
}

// TestStatusCountsReportsOfflineCount 验证 StatusCounts().Offline 正确统计
// offline=1 的资产数,且不影响 Indexed 的既有口径(offline 资产仍按 status
// 计入 Indexed,Offline 是独立叠加的统计维度)。回收站里的 offline 资产
// (deleted_at 非空 + offline=1 双标)不计入——它已经从图库里消失,不属于
// "N 张照片在已断开的磁盘上"要提示的对象。
func TestStatusCountsReportsOfflineCount(t *testing.T) {
	db := makeTestDB(t)
	insertAsset(t, db, "/DATA/a.jpg", "indexed")
	offID := insertAsset(t, db, "/media/X/b.jpg", "indexed")
	insertAsset(t, db, "/DATA/c.jpg", "pending")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offID)
	require.NoError(t, err)
	// 双标资产:先进回收站、后拔盘(或反之)——不应计入 Offline。
	dualID := insertAsset(t, db, "/media/X/trashed.jpg", "indexed")
	_, err = db.Exec(`UPDATE assets SET offline=1, deleted_at='2026-01-01 00:00:00' WHERE id=?`, dualID)
	require.NoError(t, err)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	status := ix.StatusCounts()

	require.Equal(t, 1, status.Offline, "offline=1 计数不得包含回收站双标资产")
	require.Equal(t, 3, status.Indexed, "Indexed 口径不变:offline 资产仍按 status 计入")
	require.Equal(t, 1, status.Pending)
}

// TestPruneMissingUnderLikeMetacharSiblings:`_` 是 LIKE 通配符,真实 U 盘卷标
// 就含下划线(Kingston_DataTra)。对 …/disk_A 的 prune 不得波及仅 `_` 位不同的
// 兄弟挂载 …/diskXA——旧的 `LIKE 'disk_A/%'` 会连带匹配并把兄弟盘上
// "暂时读不到"的资产物理删掉。
func TestPruneMissingUnderLikeMetacharSiblings(t *testing.T) {
	db := makeTestDB(t)
	// 夹具用真实机器上不存在的挂载名,理由同上(EACCES 会架空断言)。
	siblingID := insertAsset(t, db, "/media/nimoos-test-diskXA/photo.jpg", "indexed") // 文件不存在
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string {
		return []string{"/DATA", "/media/nimoos-test-disk_A", "/media/nimoos-test-diskXA"}
	}

	require.NoError(t, ix.pruneMissingUnder("/media/nimoos-test-disk_A"))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, siblingID).Scan(&n))
	require.Equal(t, 1, n, "prune disk_A 不得误删兄弟挂载 diskXA 的资产")
}

// TestPruneSystemMountAssetsPurgesDevmonAssets:启动清理必须把 devmon(U 盘)
// 存量资产连 CLIP 向量、人脸行一起硬删,其它路径资产不受影响。
func TestPruneSystemMountAssetsPurgesDevmonAssets(t *testing.T) {
	db := makeTestDB(t)
	usb := insertAsset(t, db, "/media/devmon/stickA/photo.jpg", "indexed")
	keep := insertAsset(t, db, "/DATA/Gallery/keep.jpg", "indexed")
	seedFaceAndClip(t, db, usb)
	seedFaceAndClip(t, db, keep)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSystemMountAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "devmon 资产行必须被硬删")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "devmon 资产的 CLIP 映射必须被清")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "devmon 资产的人脸行必须被清")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n, "非 devmon 资产不得被波及")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}

// TestPruneMissingUnderDeletedSubdir:挂载根健在、其下子目录被整体删除(Files
// 删除文件夹 → busdelete 以被删目录路径调 pruneMissingUnder)是"合法删除",
// 互锁不得把它误判成"拔盘"而放弃清理——否则该目录下所有资产永久残留,
// 还能被搜索命中(真实故障:/media/RAID_0 各相册目录被删后 81 条资产滞留)。
func TestPruneMissingUnderDeletedSubdir(t *testing.T) {
	db := makeTestDB(t)
	mountDir := t.TempDir() // 挂载根始终存在
	gonedir := filepath.Join(mountDir, "Miami")
	id := insertAsset(t, db, filepath.Join(gonedir, "photo.jpg"), "indexed") // 目录连文件已整体消失

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA", mountDir} }

	require.NoError(t, ix.pruneMissingUnder(gonedir))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, id).Scan(&n))
	require.Equal(t, 0, n, "挂载根健在时,被删子目录下的资产必须被清理")
}

func TestPruneDeleteGuard(t *testing.T) {
	root := t.TempDir()
	underRoot := func(string) (string, bool) { return root, true }
	require.True(t, pruneDeleteAllowed(root, underRoot))
	// 挂载根健在、子目录被删:合法删除,允许批删(修复前误判为拔盘)
	require.True(t, pruneDeleteAllowed(filepath.Join(root, "gone"), underRoot))
	// 目录不在任何已挂载根之下(挂载已从 /proc/mounts 消失):禁止批删
	require.False(t, pruneDeleteAllowed(root, func(string) (string, bool) { return "", false }))
	// 挂载根本体 stat 失败(死挂载残留在挂载表):禁止批删
	deadRoot := filepath.Join(root, "dead-mount")
	require.False(t, pruneDeleteAllowed(deadRoot, func(string) (string, bool) { return deadRoot, true }))
}

// TestPruneRcloneMountAssetsPurges:rclone 云盘挂载点下的历史误入库资产在
// 启动时防御性硬删;挂载点带下划线(真实命名 /mnt/yu.wu_dropbox_*)不得因
// LIKE 通配泄漏误删相邻路径资产。
func TestPruneRcloneMountAssetsPurges(t *testing.T) {
	db := makeTestDB(t)
	cloud := insertAsset(t, db, "/mnt/yu.wu_dropbox_178/photo.jpg", "indexed")
	// `_` 在 LIKE 里是单字符通配:若实现误用 LIKE,前缀 /mnt/yu.wuXdropbox... 也会被命中
	sibling := insertAsset(t, db, "/mnt/yu.wuXdropbox_178/photo.jpg", "indexed")
	keep := insertAsset(t, db, "/DATA/Gallery/keep.jpg", "indexed")

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneRcloneMountAssets([]string{"/mnt/yu.wu_dropbox_178"})

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, cloud).Scan(&n))
	require.Equal(t, 0, n, "rclone 挂载下的资产必须被清")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, sibling).Scan(&n))
	require.Equal(t, 1, n, "相邻相似路径不得被 LIKE 通配误删")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}

// TestPendingBackfillExcludesPreviewWhenDisabled:PreviewPregen 关闭
// (includePreview=false)时预扫描只按 sprite.jpg 缺失判定欠账，preview.mp4
// 缺失不再计入——它交给路由端懒生成；打开时两者都判。
func TestPendingBackfillExcludesPreviewWhenDisabled(t *testing.T) {
	thumbDir := t.TempDir()
	// 候选 a：sprite 已存在、preview 缺失 → includePreview=false 时不算欠账
	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, "a"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, "a", "sprite.jpg"), []byte("x"), 0644))
	// 候选 b：sprite 缺失 → 两种模式都算欠账
	cands := []spriteCandidate{{id: "a"}, {id: "b"}}

	got := pendingBackfill(cands, thumbDir, false)
	require.Len(t, got, 1)
	require.Equal(t, "b", got[0].id, "includePreview=false 应只剩 b")

	got = pendingBackfill(cands, thumbDir, true)
	require.Len(t, got, 2, "includePreview=true 应两条都欠")
}
