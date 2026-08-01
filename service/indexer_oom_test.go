package service

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── P1: streaming hash ────────────────────────────────────────────────────

// TestSha256FileStream_MatchesFullReadHash asserts the streaming hash
// (os.Open + io.Copy) computes exactly the same digest for the same content
// as the old "read fully into memory, then hash" approach
// (sha256File(os.ReadFile's result)) — this is the safety precondition for
// switching processFileInternal from full reads to streaming reads. The
// content deliberately crosses io.Copy's typical internal buffer size
// (32KB), covering multiple Write calls.
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
	require.Equal(t, want, got, "the streaming hash result must match the full-read hash")
}

// TestSha256FileStream_MissingFileReturnsError asserts that the streaming
// hash returns an error rather than panicking when the file doesn't exist,
// consistent with the original os.ReadFile failure behavior.
func TestSha256FileStream_MissingFileReturnsError(t *testing.T) {
	_, err := sha256FileStream(filepath.Join(t.TempDir(), "missing.bin"))
	require.Error(t, err)
}

// ── P1: MIME header-only reads ────────────────────────────────────────────

// TestDetectMimeType_KnownExtensionNeverReadsFile asserts a known extension
// (a canonicalMime table hit) returns the authoritative type directly,
// without ever attempting to open the file — using a path that flatly
// doesn't exist to prove it: if the implementation read the file, os.Open
// would fail and the function couldn't possibly return the correct result.
func TestDetectMimeType_KnownExtensionNeverReadsFile(t *testing.T) {
	got := detectMimeType("/nonexistent/does-not-exist.jpg", ".jpg")
	require.Equal(t, "image/jpeg", got, "a known extension should not attempt to read the file")
}

// TestDetectMimeType_UnknownExtensionSniffsHeader asserts that when an
// unknown extension falls back to content sniffing, only the file header is
// needed to identify the type (http.DetectContentType itself only looks at
// the first 512B), verifying detectMimeType hasn't regressed into reading
// the whole file.
func TestDetectMimeType_UnknownExtensionSniffsHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	require.NoError(t, os.WriteFile(path, pngHeader, 0o644))
	require.Equal(t, "image/png", detectMimeType(path, ".bin"), "an unknown extension should fall back to content sniffing")
}

// ── P1: image 100MB cap fallback ──────────────────────────────────────────

// TestImageExceedsReadLimit is a boundary-value unit test for the
// imageExceedsReadLimit predicate (matching the style of
// TestPixelsExceedMLLimit in mlinput_test.go): equal to the limit doesn't
// count as exceeding it, one byte over does. Uses an injected small
// threshold to test the boundary without depending on maxImageReadBytes'
// real value (production is still the 100MB constant, see the comment in
// indexer.go).
func TestImageExceedsReadLimit(t *testing.T) {
	prev := maxImageReadBytes
	t.Cleanup(func() { maxImageReadBytes = prev })
	maxImageReadBytes = 100

	require.False(t, imageExceedsReadLimit(100), "equal to the limit should not be judged as exceeding it")
	require.False(t, imageExceedsReadLimit(99))
	require.True(t, imageExceedsReadLimit(101), "over the limit should be judged as exceeding it")
}

// TestProcessFileInternal_OversizedImage_SkipsMLButIndexes covers "an
// oversized image skips ML but still completes basic indexing": injects a
// tiny threshold (to avoid actually constructing a 100MB+ file) and uses
// skipThumb to disable thumbnail generation, so embedClip can't fall back
// to small.jpg — the only thing that could be fed to ML is faceData. If
// faceData is genuinely emptied out due to exceeding the limit, CLIP/OCR
// must never be called even once, but the asset should still reach
// status='indexed' normally.
func TestProcessFileInternal_OversizedImage_SkipsMLButIndexes(t *testing.T) {
	prev := maxImageReadBytes
	t.Cleanup(func() { maxImageReadBytes = prev })
	maxImageReadBytes = 8 // the real test JPEG clearly exceeds this threshold

	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{skipThumb: true}))

	require.Zero(t, ml.clipCalls, "CLIP should not be called when faceData is emptied and there's no thumbnail fallback")
	require.Zero(t, ml.ocrCalls, "OCR should be skipped when faceData is emptied")

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, path).Scan(&status))
	require.Equal(t, "indexed", status, "an oversized image should still complete basic indexing (EXIF + DB write)")
}

// TestProcessFileInternal_NormalImage_StillRunsML is the control for the
// previous test: the same file, the same opts, just without lowering the
// threshold — ML must run as usual, confirming the new fallback branch
// hasn't accidentally affected normal-sized images.
func TestProcessFileInternal_NormalImage_StillRunsML(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{skipThumb: true}))

	require.Equal(t, 1, ml.clipCalls, "a normal-sized image should run CLIP as usual")
}

// ── P1: the video path never touches data ─────────────────────────────────

// TestProcessFileInternal_Video_StillIndexesAndEmbeds is a regression test
// after removing the top-level full read: video keyframe extraction,
// ffprobe probing, and CLIP embedding (via the keyframe file, not the whole
// video's bytes) must all behave the same as before the change.
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
	require.Greater(t, durationMs, int64(0), "video duration should be obtained via ffprobe (by path)")
	require.Equal(t, 1, ml.clipCalls, "video should go through CLIP embedding once via the keyframe file")
}

// ── P2: stat fast-skip ─────────────────────────────────────────────────────

// TestProcessFileInternal_StatFastPath_SkipsReprocessWhenUnchanged asserts:
// an asset that's already status='indexed' with unchanged file_size+mtime
// must short-circuit at the stat stage on a second processFileInternal call
// — no repeated ML call (i.e. no repeated embedClip) — this is exactly the
// key to breaking the restart death loop: knowing "this content has already
// been processed" without reading a single byte.
func TestProcessFileInternal_StatFastPath_SkipsReprocessWhenUnchanged(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "the first indexing pass should run CLIP once")

	var mtimeBefore int64
	require.NoError(t, db.QueryRow(`SELECT mtime FROM assets WHERE file_path=?`, path).Scan(&mtimeBefore))
	require.NotZero(t, mtimeBefore, "the first indexing pass should write mtime back to the DB")

	// File content and mtime are both unchanged: the second processing
	// pass should be short-circuited by the stat fast path.
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "a size+mtime hit should short-circuit and not repeat CLIP")
}

// TestProcessFileInternal_StatFastPath_ReprocessesWhenContentChanges
// asserts: when the same path's content actually changes (size/mtime/
// checksum all change), both the stat fast path and the checksum
// short-circuit should miss, and the full pipeline should rerun (ML runs
// again).
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

	writeJPEGAt(t, path, 2) // write a different image to the same path
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 2, ml.clipCalls, "a content change should trigger reprocessing, not be blocked by the stat fast path")

	var checksum string
	require.NoError(t, db.QueryRow(`SELECT checksum FROM assets WHERE file_path=?`, path).Scan(&checksum))
	require.NotEmpty(t, checksum)
}

// TestProcessFileInternal_LegacyNullMtime_BackfilledViaChecksumShortcut
// covers the backfill closed loop for legacy rows written before this
// upgrade (mtime is NULL): the stat fast path necessarily misses →
// streaming hash → hits the checksum short-circuit. Before the fix, the
// checksum short-circuit returned before the INSERT and never backfilled
// mtime, so legacy rows had to be fully re-read on every rescan; after the
// fix, the checksum short-circuit writes size+mtime back onto this
// file_path in place, so: (1) that row's mtime immediately becomes
// non-null; (2) the next processing pass hits step 2's stat fast path with
// zero reads (ML doesn't run again).
func TestProcessFileInternal_LegacyNullMtime_BackfilledViaChecksumShortcut(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	path := makeTestJPEG(t, imgDir)

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls)

	// Simulate a legacy row "written before this upgrade": wipe mtime to NULL.
	_, err := db.Exec(`UPDATE assets SET mtime=NULL WHERE file_path=?`, path)
	require.NoError(t, err)

	// Process once more: the stat fast path misses (mtime is NULL) → falls
	// through to the checksum short-circuit, content unchanged hits
	// indexed → should backfill mtime in place, without rerunning ML.
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "the checksum short-circuit should not rerun ML when content is unchanged")

	var mtime sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT mtime FROM assets WHERE file_path=?`, path).Scan(&mtime))
	require.True(t, mtime.Valid && mtime.Int64 != 0, "the checksum short-circuit should backfill the legacy row's mtime to non-null")

	// After the backfill, a third processing pass should be a zero-read hit
	// via the stat fast path (still no ML run).
	require.True(t, ix.processFileInternal(path, processOpts{}))
	require.Equal(t, 1, ml.clipCalls, "after the backfill, the stat fast path should hit and the checksum path should no longer be reached")
}

// TestProcessFileInternal_StatFastPath_IgnoredWhenForced asserts that when
// opts.force=true, the stat fast path must be bypassed and reprocessing
// forced even when size+mtime are both unchanged — consistent with the
// existing checksum short-circuit's force semantics (Embedder's CLIP
// backfill scenario).
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
	require.Equal(t, 2, ml.clipCalls, "force should bypass the stat fast path and rerun ML")
}
