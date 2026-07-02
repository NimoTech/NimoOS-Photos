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
	err := walkSupported(root, func(p string) { collected = append(collected, p) })
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

// recordingML 记录各 ML 能力被调用的次数，用于开关门控断言。
type recordingML struct {
	mockML
	clipCalls, faceCalls, ocrCalls int
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
	require.Zero(t, ml.faceCalls)
	require.Zero(t, ml.ocrCalls)

	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}
	require.True(t, ix.processFileInternal(path, processOpts{force: true}))
	require.Equal(t, 1, ml.clipCalls)
	require.Equal(t, 1, ml.faceCalls)
	require.Equal(t, 1, ml.ocrCalls)
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
