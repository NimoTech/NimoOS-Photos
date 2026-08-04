package service

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// countingML wraps mockML and counts CLIP calls; failCLIP makes embeds fail.
type countingML struct {
	mockML
	clipCalls atomic.Int64
	failCLIP  bool
}

func (m *countingML) CLIPImageEmbed(b []byte) ([]float32, error) {
	m.clipCalls.Add(1)
	if m.failCLIP {
		return nil, errAssert("clip backend down")
	}
	return m.mockML.CLIPImageEmbed(b)
}

// decodingML wraps mockML and requires a real image.Decode to succeed before
// producing an embedding — used to distinguish a genuinely-bad source file
// from a mock that would happily "succeed" on garbage bytes. Counts both
// clipCalls and ocrCalls since Task 4 gates OCR the same way.
type decodingML struct {
	mockML
	clipCalls atomic.Int64
	ocrCalls  atomic.Int64
}

func (m *decodingML) CLIPImageEmbed(b []byte) ([]float32, error) {
	m.clipCalls.Add(1)
	if _, _, err := image.Decode(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return m.mockML.CLIPImageEmbed(b)
}

func (m *decodingML) OCR(b []byte) ([]mlclient.OCRLine, error) {
	m.ocrCalls.Add(1)
	if _, _, err := image.Decode(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return m.mockML.OCR(b)
}

type errAssert string

func (e errAssert) Error() string { return string(e) }

// mlDownML wraps countingML but reports IsReady()==false, simulating a
// genuinely unreachable ML backend (as opposed to healthy-but-every-asset-
// is-broken) — used to prove the environmental guard's original protection
// survives the Task 7 fix.
type mlDownML struct {
	countingML
}

func (m *mlDownML) IsReady() bool { return false }

func writeSmallThumb(t *testing.T, thumbDir, assetID, srcJPEG string) {
	t.Helper()
	dir := filepath.Join(thumbDir, assetID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	b, err := os.ReadFile(srcJPEG)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.jpg"), b, 0o644))
}

// The core light-path assertion: with a thumbnail present, backfill must not
// touch the original file at all — deleting the source proves it.
func TestBackfill_EmbedsFromThumbWithoutReadingSource(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")
	writeSmallThumb(t, thumbDir, id, src)
	require.NoError(t, os.Remove(src)) // source gone; only the thumb remains

	ml := &countingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.hasEmbeddingForPath(src))
}

func TestBackfill_FallsBackToFullPipelineWithoutThumb(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &countingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.hasEmbeddingForPath(src)) // light path is additive, not a replacement
}

func TestQueryMissing_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	hot := insertAsset(t, db, "/hot.jpg", "indexed")
	cold := insertAsset(t, db, "/cold.jpg", "indexed")
	_ = hot
	now := time.Now()
	recordBackfillFailure(db, backfillCLIP, cold, now, errAssert("x"))

	e := NewEmbedder(db, &mockML{}, nil, NewTaskRegistry(nil))
	targets, err := e.queryMissing(context.Background(), now)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "/hot.jpg", targets[0].path)

	targets, err = e.queryMissing(context.Background(), now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 2) // first-level cooldown (5m) elapsed
}

// A failing asset gets exactly one ML attempt, then cools down. Note this
// needs >=1 succeeding asset in the same pass, otherwise the all-failed
// pass is treated as an environmental (ML-down) failure and nothing is
// recorded — that guard has its own test below.
func TestBackfill_RecordsFailureThenSkipsNextRound(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	good := makeUniqueJPEG(t, tmp, 1)
	insertAsset(t, db, good, "indexed")
	bad := filepath.Join(tmp, "bad.jpg")
	require.NoError(t, os.WriteFile(bad, []byte("not a jpeg"), 0o644))
	badID := insertAsset(t, db, bad, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &decodingML{} // real image.Decode — bad.jpg fails, good succeeds
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))

	require.NoError(t, e.Backfill(context.Background()))
	n, _ := readBackfillFailure(t, db, backfillCLIP, badID)
	require.Equal(t, 1, n)

	before := ml.clipCalls.Load()
	require.NoError(t, e.Backfill(context.Background()))
	require.Equal(t, before, ml.clipCalls.Load()) // cooled-down asset not re-attempted
}

// Environmental guard: when the whole pass fails with zero successes AND ML
// is genuinely unreachable (the existing TaskErrMLLostDuringBackfill
// condition), no per-asset failures may be recorded — a dead ML backend must
// not walk healthy assets up the ladder. Uses mlDownML (IsReady()==false) to
// simulate a genuinely dead backend, distinct from an all-corrupt asset set
// on a healthy backend (see TestBackfill_AllCorruptPassConvergesWhenMLReady).
func TestBackfill_AllFailedPassRecordsNothing(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &mlDownML{countingML: countingML{failCLIP: true}}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	_ = e.Backfill(context.Background())
	n, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Zero(t, n)
}

func TestBackfill_ClearsFailureAfterSuccess(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")
	recordBackfillFailure(db, backfillCLIP, id,
		time.Now().Add(-48*time.Hour), errAssert("stale")) // old failure, cooldown long over

	ml := &countingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.Backfill(context.Background()))
	n, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Zero(t, n)
}

// TestBackfill_ThumbPathMLErrorDoesNotFallThroughToSource locks the binding
// constraint embedOne's doc comment claims: a light-path ML error must NOT
// fall through to ForceReprocess. A naive "does the final embed still fail"
// check can't tell the two apart here: embedClip always prefers an existing
// small.jpg thumb file over any fallback bytes a full reprocess would hand
// it, so even a regressed fallthrough would still fail to embed via the same
// (still-present) garbage thumb. The real tell is a side effect ONLY
// ForceReprocess produces: processFileInternal recomputes the file's
// checksum from the real source bytes and unconditionally overwrites
// assets.checksum via `INSERT ... ON CONFLICT(file_path) DO UPDATE SET
// checksum=excluded.checksum` (indexer.go, ~line 1031), regardless of how
// the downstream CLIP call turns out. So: keep the source file real and
// readable (not deleted, unlike the light-path-success test above), make
// only the *thumb* undecodable so the light-path CLIP call fails, and assert
// the checksum recorded at insertAsset time is untouched afterward — proving
// ForceReprocess (the only path that would rewrite it) never ran.
func TestBackfill_ThumbPathMLErrorDoesNotFallThroughToSource(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")

	// `good` has no thumb, so it takes the full pipeline and succeeds —
	// keeping the environmental (all-failed) guard from suppressing the
	// failure record below (TestBackfill_AllFailedPassRecordsNothing).
	good := makeUniqueJPEG(t, tmp, 1)
	insertAsset(t, db, good, "indexed")

	bad := makeUniqueJPEG(t, tmp, 2) // real, readable source — deliberately NOT deleted
	badID := insertAsset(t, db, bad, "indexed")
	var origChecksum string
	require.NoError(t, db.QueryRow(`SELECT checksum FROM assets WHERE id=?`, badID).Scan(&origChecksum))

	// The thumb is garbage: image.Decode fails on it, so decodingML's
	// light-path CLIP call fails for this asset specifically.
	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, badID), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, badID, "small.jpg"), []byte("not a jpeg"), 0o644))

	ml := &decodingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.Backfill(context.Background()))

	n, _ := readBackfillFailure(t, db, backfillCLIP, badID)
	require.Equal(t, 1, n, "the thumb-path ML failure should be recorded")
	require.False(t, e.hasEmbeddingForPath(bad), "no embedding should exist for the failed asset")

	var checksumAfter string
	require.NoError(t, db.QueryRow(`SELECT checksum FROM assets WHERE id=?`, badID).Scan(&checksumAfter))
	require.Equal(t, origChecksum, checksumAfter,
		"checksum must stay untouched — ForceReprocess (the only code path that rewrites it) must never run when a thumb is present")
}

// TestBackfill_AllCorruptPassConvergesWhenMLReady closes the convergence gap:
// a pass where every asset fails (e.g. a folder of 100% corrupt recovery
// artifacts) but the ML backend itself is healthy must still record each
// failure, or the environmental (all-failed) guard mistakes "assets are
// broken" for "ML is down" and the same corrupt set gets re-read every gate
// window forever. decodingML reports IsReady()==true (inherited from
// mockML) while still genuinely failing to decode the garbage bytes below.
func TestBackfill_AllCorruptPassConvergesWhenMLReady(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")

	const n = 3
	var ids []string
	for i := 0; i < n; i++ {
		bad := filepath.Join(tmp, fmt.Sprintf("corrupt%d.jpg", i))
		require.NoError(t, os.WriteFile(bad, []byte("not a jpeg"), 0o644))
		ids = append(ids, insertAsset(t, db, bad, "indexed"))
	}

	ml := &decodingML{} // IsReady()==true; every asset genuinely fails to decode
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))

	require.NoError(t, e.Backfill(context.Background()))
	require.NoError(t, e.Backfill(context.Background()))

	leftover, err := e.queryMissing(context.Background(), time.Now())
	require.NoError(t, err)
	require.Empty(t, leftover, "an all-corrupt pass on a healthy ML backend must still converge")

	for _, id := range ids {
		n, _ := readBackfillFailure(t, db, backfillCLIP, id)
		require.Equal(t, 1, n, "%s: recorded once, not skipped by the environmental guard", id)
	}
}

// TestBackfill_MLDownPassStillRecordsNothing preserves the original
// protection: when the ML backend is genuinely unreachable (IsReady()==false)
// an all-failed pass must NOT record per-asset failures, since that would
// walk healthy assets up the cooldown ladder while ML is merely down.
func TestBackfill_MLDownPassStillRecordsNothing(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &mlDownML{countingML: countingML{failCLIP: true}}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.Backfill(context.Background()))

	n, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Zero(t, n, "ML-down pass must not record any per-asset failure")
}

func TestQueryMissingOCR_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	hot := insertAsset(t, db, "/hot.jpg", "indexed")
	cold := insertAsset(t, db, "/cold.jpg", "indexed")
	_ = hot
	now := time.Now()
	recordBackfillFailure(db, backfillOCR, cold, now, errAssert("x"))

	e := NewEmbedder(db, &mockML{}, nil, NewTaskRegistry(nil))
	ts, err := e.queryMissingOCR(context.Background(), now)
	require.NoError(t, err)
	require.Len(t, ts, 1)
	ts, err = e.queryMissingOCR(context.Background(), now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, ts, 2)
}

// OCR failure is recorded once from the FINAL pass only (the built-in
// cold-start retry runs the pass twice); the next round skips the asset.
// Needs one succeeding asset so the pass isn't classified as ML-down.
func TestBackfillOCR_RecordsFailureThenSkipsNextRound(t *testing.T) {
	prev := ocrBackfillRetryDelay
	ocrBackfillRetryDelay = time.Millisecond
	t.Cleanup(func() { ocrBackfillRetryDelay = prev })

	db := makeTestDB(t)
	tmp := t.TempDir()
	good := makeUniqueJPEG(t, tmp, 1)
	insertAsset(t, db, good, "indexed")
	bad := filepath.Join(tmp, "bad.jpg")
	require.NoError(t, os.WriteFile(bad, []byte("not a jpeg"), 0o644))
	badID := insertAsset(t, db, bad, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &decodingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.BackfillOCR(context.Background()))
	n, _ := readBackfillFailure(t, db, backfillOCR, badID)
	require.Equal(t, 1, n)

	before := ml.ocrCalls.Load()
	require.NoError(t, e.BackfillOCR(context.Background()))
	require.Equal(t, before, ml.ocrCalls.Load())
}

// TestBackfillOCR_AllCorruptPassConvergesWhenMLReady mirrors
// TestBackfill_AllCorruptPassConvergesWhenMLReady for the OCR chain: an
// all-failed OCR pass with a healthy ML backend must still record each
// asset's failure so the corrupt set converges after two rounds.
func TestBackfillOCR_AllCorruptPassConvergesWhenMLReady(t *testing.T) {
	prev := ocrBackfillRetryDelay
	ocrBackfillRetryDelay = time.Millisecond
	t.Cleanup(func() { ocrBackfillRetryDelay = prev })

	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")

	const n = 3
	var ids []string
	for i := 0; i < n; i++ {
		bad := filepath.Join(tmp, fmt.Sprintf("corrupt%d.jpg", i))
		require.NoError(t, os.WriteFile(bad, []byte("not a jpeg"), 0o644))
		ids = append(ids, insertAsset(t, db, bad, "indexed"))
	}

	ml := &decodingML{} // IsReady()==true; every asset genuinely fails to OCR
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))

	require.NoError(t, e.BackfillOCR(context.Background()))
	require.NoError(t, e.BackfillOCR(context.Background()))

	leftover, err := e.queryMissingOCR(context.Background(), time.Now())
	require.NoError(t, err)
	require.Empty(t, leftover, "an all-corrupt OCR pass on a healthy ML backend must still converge")

	for _, id := range ids {
		n, _ := readBackfillFailure(t, db, backfillOCR, id)
		require.Equal(t, 1, n, "%s: recorded once, not skipped by the environmental guard", id)
	}
}

func TestBackfillOCR_ClearsFailureAfterSuccess(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")
	recordBackfillFailure(db, backfillOCR, id, time.Now().Add(-48*time.Hour), errAssert("stale"))

	ml := &countingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
	require.NoError(t, e.BackfillOCR(context.Background()))
	n, _ := readBackfillFailure(t, db, backfillOCR, id)
	require.Zero(t, n)
}

// insertVideoAsset inserts a minimal indexed video row for sprite-backfill
// ledger tests (spriteBackfillCandidates requires mime_type LIKE 'video/%',
// status='indexed', and duration_ms>0).
func insertVideoAsset(t *testing.T, db *sql.DB, path string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
		is_live_photo_video, status, checksum, duration_ms)
		VALUES(?,?,?, 'video/mp4', ?, 0, 'indexed', ?, 5000)`,
		id, path, 1234, filepath.Base(path), uuid.NewString())
	require.NoError(t, err)
	return id
}

func TestSpriteBackfillCandidates_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	insertVideoAsset(t, db, "/hot.mp4")
	cold := insertVideoAsset(t, db, "/cold.mp4")
	now := time.Now()
	recordBackfillFailure(db, backfillSprite, cold, now, errAssert("x"))

	cs, err := spriteBackfillCandidates(db, now)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	cs, err = spriteBackfillCandidates(db, now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, cs, 2)
}

// A video that ffmpeg cannot process gets one attempt then cools down —
// no more re-invoking ffmpeg on the same broken file every batch.
func TestBackfillSprites_RecordsFailureThenSkipsNextRound(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	db := makeTestDB(t)
	tmp := t.TempDir()
	broken := filepath.Join(tmp, "broken.mp4")
	require.NoError(t, os.WriteFile(broken, []byte("not actually an mp4"), 0o644))
	id := insertVideoAsset(t, db, broken)

	ix := NewIndexer(db, &mockML{}, filepath.Join(tmp, "thumbs"), 1)
	ix.SetTaskRegistry(NewTaskRegistry(nil))
	ix.BackfillSprites(context.Background())
	n, _ := readBackfillFailure(t, db, backfillSprite, id)
	require.Equal(t, 1, n)

	ix.BackfillSprites(context.Background())
	n, _ = readBackfillFailure(t, db, backfillSprite, id)
	require.Equal(t, 1, n) // second round skipped it entirely (still 1, not 2)
}
