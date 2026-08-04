package service

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
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

// Environmental guard: when the whole pass fails with zero successes (the
// existing TaskErrMLLostDuringBackfill condition), no per-asset failures may
// be recorded — a dead ML backend must not walk healthy assets up the ladder.
func TestBackfill_AllFailedPassRecordsNothing(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	thumbDir := filepath.Join(tmp, "thumbs")

	ml := &countingML{failCLIP: true}
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
