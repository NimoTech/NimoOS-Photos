package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── P1：流式哈希 ──────────────────────────────────────────────────────────

// TestSha256FileStream_MatchesFullReadHash 断言流式哈希（os.Open + io.Copy）
// 与原来"整读进内存再算"（sha256File(os.ReadFile 的结果)）对同一份内容算出
// 完全相同的摘要——这是把 processFileInternal 从整读切到流式读取的安全前提。
// 内容特意跨过典型的 io.Copy 内部缓冲区大小（32KB），覆盖多次 Write 调用。
func TestSha256FileStream_MatchesFullReadHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	content := make([]byte, 1<<20) // 1MB
	for i := range content {
		content[i] = byte(i * 31 % 251)
	}
	require.NoError(t, os.WriteFile(path, content, 0o644))

	want := sha256File(content)
	got, err := sha256FileStream(path)
	require.NoError(t, err)
	require.Equal(t, want, got, "流式哈希结果必须与整读哈希一致")
}

// TestSha256FileStream_MissingFileReturnsError 断言文件不存在时流式哈希返回
// 错误而不是 panic，与原 os.ReadFile 失败时的行为一致。
func TestSha256FileStream_MissingFileReturnsError(t *testing.T) {
	_, err := sha256FileStream(filepath.Join(t.TempDir(), "missing.bin"))
	require.Error(t, err)
}

// ── P1：MIME 只读头部 ─────────────────────────────────────────────────────

// TestDetectMimeType_KnownExtensionNeverReadsFile 断言已知扩展名（命中
// canonicalMime 表）直接返回权威类型，完全不尝试打开文件——用一个根本不存在
// 的路径来证明：如果实现读了文件，os.Open 会失败，函数就不可能返回正确结果。
func TestDetectMimeType_KnownExtensionNeverReadsFile(t *testing.T) {
	got := detectMimeType("/nonexistent/does-not-exist.jpg", ".jpg")
	require.Equal(t, "image/jpeg", got, "已知扩展名不应尝试读取文件")
}

// TestDetectMimeType_UnknownExtensionSniffsHeader 断言未知扩展名回退到内容
// 嗅探时只需要文件头部就能识别类型（http.DetectContentType 本身只看前
// 512B），验证 detectMimeType 没有退化成整文件读入。
func TestDetectMimeType_UnknownExtensionSniffsHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	require.NoError(t, os.WriteFile(path, pngHeader, 0o644))
	require.Equal(t, "image/png", detectMimeType(path, ".bin"), "未知扩展名应回退到内容嗅探")
}

// ── P1：图片 100MB 上限降级 ───────────────────────────────────────────────

// TestImageExceedsReadLimit 是 imageExceedsReadLimit 判定函数的边界值单测
// （对齐 mlinput_test.go 里 TestPixelsExceedMLLimit 的写法）：等于上限不算
// 超限，超过一个字节才算超限。用注入的小阈值验证边界，不依赖 maxImageReadBytes
// 的真实取值（生产环境仍是 100MB 常量，见 indexer.go 的注释）。
func TestImageExceedsReadLimit(t *testing.T) {
	prev := maxImageReadBytes
	t.Cleanup(func() { maxImageReadBytes = prev })
	maxImageReadBytes = 100

	require.False(t, imageExceedsReadLimit(100), "等于上限不应判定超限")
	require.False(t, imageExceedsReadLimit(99))
	require.True(t, imageExceedsReadLimit(101), "超过上限应判定超限")
}

// TestProcessFileInternal_OversizedImage_SkipsMLButIndexes 覆盖"超大图跳过
// ML 仍完成基础索引"：注入一个极小的阈值（避免真的构造 100MB+ 文件），并用
// skipThumb 关掉缩略图生成，这样 embedClip 找不到 small.jpg 兜底，唯一能喂给
// ML 的就是 faceData——如果 faceData 真的因为超限被置空，CLIP/OCR 必然一次
// 都不会被调用，但资产仍应正常走到 status='indexed'。
func TestProcessFileInternal_OversizedImage_SkipsMLButIndexes(t *testing.T) {
	prev := maxImageReadBytes
	t.Cleanup(func() { maxImageReadBytes = prev })
	maxImageReadBytes = 8 // 真实测试 JPEG 显然超过这个阈值

	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{skipThumb: true}))

	require.Zero(t, ml.clipCalls, "faceData 置空且无缩略图兜底时不应调用 CLIP")
	require.Zero(t, ml.ocrCalls, "faceData 置空时应跳过 OCR")

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, path).Scan(&status))
	require.Equal(t, "indexed", status, "超大图仍应完成基础索引（EXIF+入库)")
}

// TestProcessFileInternal_NormalImage_StillRunsML 是上一测试的对照组：同一份
// 文件、同一份 opts，只是没有调低阈值，ML 必须照常跑——确保新增的降级分支
// 没有误伤正常大小的图片。
func TestProcessFileInternal_NormalImage_StillRunsML(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{skipThumb: true}))

	require.Equal(t, 1, ml.clipCalls, "正常大小的图片应照常跑 CLIP")
}

// ── P1：视频路径完全不碰 data ─────────────────────────────────────────────

// TestProcessFileInternal_Video_StillIndexesAndEmbeds 是移除顶层整读之后的
// 回归测试：视频的关键帧提取、ffprobe 探测、CLIP 嵌入（走关键帧文件，不走
// 整段视频字节）都必须与改动前行为一致。
func TestProcessFileInternal_Video_StillIndexesAndEmbeds(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	vidDir := t.TempDir()
	path := makeTestVideo(t, vidDir, "clip.mp4")

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var status, mime string
	var durationMs int64
	require.NoError(t, db.QueryRow(
		`SELECT status, mime_type, duration_ms FROM assets WHERE file_path=?`, path,
	).Scan(&status, &mime, &durationMs))
	require.Equal(t, "indexed", status)
	require.Equal(t, "video/mp4", mime)
	require.Greater(t, durationMs, int64(0), "视频时长应通过 ffprobe（按路径）取得")
	require.Equal(t, 1, ml.clipCalls, "视频应通过关键帧文件走一次 CLIP 嵌入")
}

// ── P2：stat 快速跳过 ─────────────────────────────────────────────────────

// TestProcessFileInternal_StatFastPath_SkipsReprocessWhenUnchanged 断言：
// 已经 status='indexed' 且 file_size+mtime 都没变的资产，再次
// processFileInternal 必须在 stat 阶段就短路——不重复调用 ML（也就是不重复
// embedClip），这正是打破重启死循环的关键：不用读一个字节就知道"这份内容
// 早就处理过"。
func TestProcessFileInternal_StatFastPath_SkipsReprocessWhenUnchanged(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "首次索引应跑一次 CLIP")

	var mtimeBefore int64
	require.NoError(t, db.QueryRow(`SELECT mtime FROM assets WHERE file_path=?`, path).Scan(&mtimeBefore))
	require.NotZero(t, mtimeBefore, "首次索引应把 mtime 回填入库")

	// 文件内容、mtime 都没变：第二次处理应该被 stat 快速路径短路。
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "size+mtime 命中应短路，不重复 CLIP")
}

// TestProcessFileInternal_StatFastPath_ReprocessesWhenContentChanges 断言：
// 同一路径的内容真的变了（size/mtime/checksum 都会变化）时，stat 快速路径和
// checksum 短路都应该 miss，重新走一遍完整流水线（重新跑 ML）。
func TestProcessFileInternal_StatFastPath_ReprocessesWhenContentChanges(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := filepath.Join(imgDir, "a.jpg")
	writeJPEGAt(t, path, 1)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls)

	writeJPEGAt(t, path, 2) // 换一张不同内容的图片写到同一路径
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 2, ml.clipCalls, "内容变化应重新处理，不能被 stat 快速路径挡住")

	var checksum string
	require.NoError(t, db.QueryRow(`SELECT checksum FROM assets WHERE file_path=?`, path).Scan(&checksum))
	require.NotEmpty(t, checksum)
}

// TestProcessFileInternal_StatFastPath_IgnoredWhenForced 断言 opts.force=true
// 时即便 size+mtime 都没变也必须绕过 stat 快速路径重新跑一遍——与既有的
// checksum 短路 force 语义保持一致（Embedder 的 CLIP 补跑场景）。
func TestProcessFileInternal_StatFastPath_IgnoredWhenForced(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls)

	ok := ix.ForceReprocess(path, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)
	require.Equal(t, 2, ml.clipCalls, "force 应绕过 stat 快速路径重新跑 ML")
}
