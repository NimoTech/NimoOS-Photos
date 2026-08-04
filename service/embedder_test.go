package service

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func insertAsset(t *testing.T, db *sql.DB, path string, status string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
        INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
                           is_live_photo_video, status, checksum)
        VALUES(?,?,?, 'image/jpeg', ?, 0, ?, ?)`,
		id, path, 1, path, status, uuid.NewString())
	require.NoError(t, err)
	return id
}

func insertClipIdx(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)
}

// TestEmbedder_QueryMissing only returns assets that are status='indexed' and have no row in asset_clip_idx.
func TestEmbedder_QueryMissing(t *testing.T) {
	db := makeTestDB(t)
	missing := insertAsset(t, db, "/a.jpg", "indexed")
	_ = insertAsset(t, db, "/b.jpg", "pending") // should not be returned
	hasIdx := insertAsset(t, db, "/c.jpg", "indexed")
	insertClipIdx(t, db, hasIdx) // already has an idx row, should not be returned

	e := NewEmbedder(db, &mockML{}, nil, nil)
	targets, err := e.queryMissing(context.Background(), time.Now())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "/a.jpg", targets[0].path)
	_ = missing
}

// TestEmbedder_QueryMissingExcludesOffline verifies: when an asset's
// removable drive has been unplugged (offline=1), it must not enter the
// CLIP backfill targets even with a missing asset_clip_idx row — the
// source file can't be read, so a backfill attempt would only keep
// failing; MountGuard proactively re-triggers Backfill once the drive is
// replugged.
func TestEmbedder_QueryMissingExcludesOffline(t *testing.T) {
	db := makeTestDB(t)
	online := insertAsset(t, db, "/a.jpg", "indexed")
	offline := insertAsset(t, db, "/media/X/b.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offline)
	require.NoError(t, err)

	e := NewEmbedder(db, &mockML{}, nil, nil)
	targets, err := e.queryMissing(context.Background(), time.Now())
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "/a.jpg", targets[0].path)
	_ = online
}

// TestEmbedder_QueryMissingOCRExcludesOffline: same as above, for the OCR backfill target query.
func TestEmbedder_QueryMissingOCRExcludesOffline(t *testing.T) {
	db := makeTestDB(t)
	online := insertAsset(t, db, "/photo-online.jpg", "indexed")
	offline := insertAsset(t, db, "/media/X/photo-offline.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offline)
	require.NoError(t, err)

	e := NewEmbedder(db, &mockML{}, nil, nil)
	targets, err := e.queryMissingOCR(context.Background())
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, tg := range targets {
		ids[tg.id] = true
	}
	require.True(t, ids[online], "an online asset should be an OCR backfill target")
	require.False(t, ids[offline], "an offline asset must be excluded from the OCR backfill")
}

// TestQueryMissingOCRIncludesLegacyBoxless verifies the backfill filter
// covers legacy assets that "have OCR text but boxes_ver=0 (coordinates
// missing)", and that they drop out of the queue once boxes_ver=1.
func TestQueryMissingOCRIncludesLegacyBoxless(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status) VALUES
		('legacy', '/g/legacy.jpg', 'image/jpeg', 'indexed'),
		('fresh',  '/g/fresh.jpg',  'image/jpeg', 'indexed')`)
	require.NoError(t, err)
	// legacy: old-version OCR — has text and coverage, but boxes_ver=0.
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, boxes_ver)
		VALUES('legacy', 'hello', 0.05, 1, 0)`)
	require.NoError(t, err)
	// fresh: new-version OCR — boxes_ver=1, should not be queued.
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, boxes_ver)
		VALUES('fresh', 'world', 0.05, 1, 1)`)
	require.NoError(t, err)

	e := NewEmbedder(db, &mockML{}, nil, nil)
	targets, err := e.queryMissingOCR(context.Background())
	require.NoError(t, err)

	ids := make([]string, 0, len(targets))
	for _, tg := range targets {
		ids = append(ids, tg.id)
	}
	require.Equal(t, []string{"legacy"}, ids)
}

// Videos don't participate in OCR: they neither enter the backfill targets,
// nor do legacy video OCR rows survive — pruneVideoOCR clears those too.
func TestVideoOCRExcludedAndPruned(t *testing.T) {
	db := makeTestDB(t)
	img := insertAsset(t, db, "/photo.jpg", "indexed") // image: missing OCR → should be a backfill target
	vid := uuid.NewString()
	_, err := db.Exec(`INSERT INTO assets(id,file_path,file_size,mime_type,original_name,is_live_photo_video,status,checksum)
		VALUES(?, '/clip.mp4', 1, 'video/mp4', 'clip.mp4', 0, 'indexed', ?)`, vid, uuid.NewString())
	require.NoError(t, err)

	e := NewEmbedder(db, &mockML{}, nil, nil)
	targets, err := e.queryMissingOCR(context.Background())
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, tg := range targets {
		ids[tg.id] = true
	}
	require.True(t, ids[img], "an image missing OCR should be a backfill target")
	require.False(t, ids[vid], "videos must be excluded from the OCR backfill")

	// pruneVideoOCR deletes video OCR rows, keeps image OCR rows.
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES(?, ?)`, vid, "spreadsheet text")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES(?, ?)`, img, "receipt")
	require.NoError(t, err)
	pruneVideoOCR(db)
	var vidRows, imgRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_ocr WHERE asset_id=?`, vid).Scan(&vidRows))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_ocr WHERE asset_id=?`, img).Scan(&imgRows))
	require.Equal(t, 0, vidRows, "the video's OCR row should be cleared")
	require.Equal(t, 1, imgRows, "the image's OCR row should be kept")
}

// gateML's CLIPImageEmbed blocks on its first call until release, then
// (including that first call) always succeeds like mockML — used to create
// a "Backfill running for a long time" window without leaving the asset
// permanently unembeddable (the cooldown ledger would otherwise legitimately
// block a same-asset retry observation, per TestQueryMissing_SkipsAssetsInCooldown).
type gateML struct {
	mockML
	clipCalls atomic.Int32
	entered   chan struct{}
	release   chan struct{}
}

func (m *gateML) CLIPImageEmbed(b []byte) ([]float32, error) {
	if m.clipCalls.Add(1) == 1 {
		m.entered <- struct{}{}
		<-m.release
	}
	return m.mockML.CLIPImageEmbed(b)
}

// TestEmbedder_BackfillRerunsWhenTriggeredMidRun verifies the
// rerun-pending mechanism: when a second call arrives while Backfill is
// already running, it can't be silently swallowed by losing the CAS like
// before — the in-progress pass may have already queried its target list
// and wouldn't see an asset that just became backfillable (typically:
// MountGuard just marked a replugged drive back to online). The second
// call should set pending, so the current pass automatically requeries and
// runs another round once it finishes — proved here by inserting a new
// asset while the first pass is blocked mid-flight (after it already
// queried its targets) and confirming only the rerun pass could have
// embedded it. (The cooldown ledger now legitimately blocks the old
// observation of "the same failing asset gets retried", since a failed
// asset cools down instead of being retried on the very next pass.)
func TestEmbedder_BackfillRerunsWhenTriggeredMidRun(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	// Only enable CLIP: faces/OCR are off, to avoid unrelated ML calls interfering with the count.
	config.Cfg = &config.Config{ScenesEnabled: true}

	db := makeTestDB(t)
	tmp := t.TempDir()
	path := makeUniqueJPEG(t, tmp, 0)
	insertAsset(t, db, path, "indexed") // missing CLIP vector → a Backfill target

	ml := &gateML{entered: make(chan struct{}, 1), release: make(chan struct{})}
	idx := NewIndexer(db, ml, t.TempDir(), 1)
	e := NewEmbedder(db, ml, idx, NewTaskRegistry(nil))

	errCh := make(chan error, 1)
	go func() { errCh <- e.Backfill(context.Background()) }()
	<-ml.entered // the first pass has entered the ML call: Backfill is confirmed running

	// Second trigger while running: returns nil immediately, but must set rerunPending instead of being swallowed.
	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.rerunPending.Load(), "a trigger received while running must set rerunPending")

	// Insert a new asset while the first pass is blocked: it didn't exist
	// when the first pass queried its targets, so only the rerun pass
	// (triggered by rerunPending) can pick it up.
	newPath := makeUniqueJPEG(t, tmp, 1)
	insertAsset(t, db, newPath, "indexed")

	close(ml.release)
	require.NoError(t, <-errCh)

	require.True(t, e.hasEmbeddingForPath(newPath),
		"the rerun pass triggered by rerunPending should have requeried and embedded the newly inserted asset")
	require.False(t, e.rerunPending.Load(), "pending should be consumed after the rerun round finishes")
	require.False(t, e.running.Load())
}

// TestEmbedder_BackfillOCRRerunPendingConsumedAfterRun verifies the same
// rerun-pending mechanism for the OCR backfill (shares the same loop shape
// with Backfill; this only verifies the CAS/pending being set and consumed).
func TestEmbedder_BackfillOCRRerunPendingConsumedAfterRun(t *testing.T) {
	db := makeTestDB(t)
	e := NewEmbedder(db, &mockML{}, nil, nil)

	// Simulate an OCR backfill round already running: a trigger arriving now must set pending, not be swallowed.
	e.ocrRunning.Store(true)
	require.NoError(t, e.BackfillOCR(context.Background()))
	require.True(t, e.ocrRerunPending.Load(), "an OCR trigger received while running must set ocrRerunPending")
	e.ocrRunning.Store(false)

	// The next actual run consumes pending and runs one more round (both rounds complete immediately on an empty DB).
	require.NoError(t, e.BackfillOCR(context.Background()))
	require.False(t, e.ocrRerunPending.Load(), "pending should be consumed once the backfill finishes")
}

// TestEmbedder_HasEmbeddingForPath
func TestEmbedder_HasEmbeddingForPath(t *testing.T) {
	db := makeTestDB(t)
	a := insertAsset(t, db, "/x.jpg", "indexed")
	insertClipIdx(t, db, a)
	_ = insertAsset(t, db, "/y.jpg", "indexed")

	e := NewEmbedder(db, &mockML{}, nil, nil)
	require.True(t, e.hasEmbeddingForPath("/x.jpg"))
	require.False(t, e.hasEmbeddingForPath("/y.jpg"))
	require.False(t, e.hasEmbeddingForPath("/nope.jpg"))
}

// flakyML: the Nth CLIPImageEmbed call returns an error, the rest return normal vectors.
type flakyML struct {
	mockML
	failOnCalls map[int]bool
	calls       atomic.Int64
}

func (m *flakyML) CLIPImageEmbed(d []byte) ([]float32, error) {
	n := int(m.calls.Add(1))
	if m.failOnCalls[n] {
		return nil, fmt.Errorf("simulated ml failure")
	}
	return m.mockML.CLIPImageEmbed(d)
}

// makeUniqueJPEG generates a JPEG with unique content (different idx →
// different checksum), for use by Backfill tests. Unlike makeTestJPEGNamed:
// fills with an idx-derived color to avoid every file having the same checksum.
func makeUniqueJPEG(t *testing.T, dir string, idx int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	c := color.RGBA{R: uint8(idx * 50 % 256), G: uint8(idx * 30 % 256), B: uint8(idx * 70 % 256), A: 255}
	for y := 0; y < 50; y++ {
		for x := 0; x < 50; x++ {
			img.Set(x, y, c)
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("u%d.jpg", idx))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return path
}

// TestEmbedder_Backfill_AllSuccess: 5 missing vectors → done, current=5
func TestEmbedder_Backfill_AllSuccess(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	// Use the indexer to get images to indexed-without-embedding first: run once with ML not ready
	idx := NewIndexer(db, &mockMLNotReady{}, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	var paths []string
	for i := 0; i < 5; i++ {
		p := makeUniqueJPEG(t, imgDir, i)
		paths = append(paths, p)
		idx.Enqueue(p)
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 5
	}, 10*time.Second, 100*time.Millisecond)

	// Now start the embedder with a ready ML
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, &mockML{}, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv, "there should be a done event")
	require.Equal(t, int64(5), doneEv.Current)
	require.Equal(t, "Generating AI index", doneEv.Label)
}

// TestEmbedder_Backfill_PartialFail: 3 succeed + 2 fail → done, label contains "(2 failed)"
func TestEmbedder_Backfill_PartialFail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	for i := 0; i < 5; i++ {
		idx.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 5
	}, 10*time.Second, 100*time.Millisecond)

	flaky := &flakyML{failOnCalls: map[int]bool{2: true, 4: true}}
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, flaky, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, flaky, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv)
	require.Contains(t, doneEv.Label, "2 failed")
}

// TestEmbedder_Backfill_AllFail: 0 succeed + N fail → error
func TestEmbedder_Backfill_AllFail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	for i := 0; i < 3; i++ {
		idx.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 3
	}, 10*time.Second, 100*time.Millisecond)

	allFail := &flakyML{failOnCalls: map[int]bool{1: true, 2: true, 3: true}}
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, allFail, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, allFail, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var errEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "error" {
			errEv = &emitted[i]
		}
	}
	require.NotNil(t, errEv, "an error event should be published when everything fails")
	require.Contains(t, errEv.Error, "ML")
}

// TestEmbedder_Backfill_CtxCancelMidwayDoesNotEmitDone:
// when ctx is cancelled midway through the loop, a "done"-status final task
// must not be published, and context.Canceled should be returned.
//
// Strategy:
//   - Insert 10 real JPEGs, letting Indexer first turn them into
//     status='indexed' (no CLIP vector).
//   - Use slowML (adds a 50ms delay per CLIPImageEmbed call) so the whole
//     loop takes ~500ms.
//   - ctx times out after 150ms, by which point roughly 2-3 have been
//     processed, triggering break.
//   - Before the fix: break fell straight into the final-state decision →
//     published done; after the fix: checks ctx.Err() → returns, no done published.
func TestEmbedder_Backfill_CtxCancelMidwayDoesNotEmitDone(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	// Use mockMLNotReady to first get the images indexed (no CLIP vector)
	notReady := &mockMLNotReady{}
	idx0 := NewIndexer(db, notReady, thumbDir, 1)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go idx0.Start(bgCtx)
	for i := 0; i < 10; i++ {
		idx0.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 10
	}, 15*time.Second, 100*time.Millisecond, "waiting for 10 assets to be indexed")

	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(tk Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })

	// slowML: adds a 50ms delay to each CLIPImageEmbed call, ~500ms total for 10 files
	slow := &slowML{delay: 50 * time.Millisecond}
	idx2 := NewIndexer(db, slow, thumbDir, 1)
	go idx2.Start(bgCtx)
	e := NewEmbedder(db, slow, idx2, reg)

	// Cancel after 150ms, by which point the loop has only run 2-3 rounds, nowhere near all 10
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := e.Backfill(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "should return DeadlineExceeded once ctx times out")

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range emitted {
		if ev.Type == "embedding" && ev.Status == "done" {
			t.Fatalf("should not publish a done event after ctx is cancelled: %+v", ev)
		}
	}
}

// slowML wraps mockML, adding a fixed delay to CLIPImageEmbed.
type slowML struct {
	mockML
	delay time.Duration
}

func (m *slowML) CLIPImageEmbed(d []byte) ([]float32, error) {
	time.Sleep(m.delay)
	return m.mockML.CLIPImageEmbed(d)
}

// TestEmbedder_Backfill_ConcurrencyGuard calls Backfill twice at once; the second returns instantly.
func TestEmbedder_Backfill_ConcurrencyGuard(t *testing.T) {
	db := makeTestDB(t)
	_ = insertAsset(t, db, "/a.jpg", "indexed") // 1 asset missing a vector

	e := NewEmbedder(db, &mockML{}, nil /* indexer is not called in this test */, NewTaskRegistry(nil))

	e.running.Store(true)
	err := e.Backfill(context.Background())
	require.NoError(t, err, "should return nil instantly while already running")
	e.running.Store(false)

	db2 := makeTestDB(t)
	e2 := NewEmbedder(db2, &mockML{}, nil, NewTaskRegistry(nil))
	require.NoError(t, e2.Backfill(context.Background()))
}

// togglingML: its IsReady return value is controlled externally via an atomic.
type togglingML struct {
	mockML
	ready atomic.Bool
}

func (m *togglingML) IsReady() bool { return m.ready.Load() }

// TestEmbedder_Run_TriggersOnReadyJump: a Backfill fires once on ML ready's false→true transition.
func TestEmbedder_Run_TriggersOnReadyJump(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	idx.Enqueue(makeUniqueJPEG(t, imgDir, 0))
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 1
	}, 5*time.Second, 50*time.Millisecond)

	tog := &togglingML{}
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, tog, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, tog, idx2, reg)
	e.SetPollInterval(50 * time.Millisecond)

	go e.Run(ctx)
	// Should not trigger while ready=false
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	require.Empty(t, embeddingTasks(emitted), "Backfill should not trigger while ML isn't ready")
	mu.Unlock()

	// Flip ready=true
	tog.ready.Store(true)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range emitted {
			if ev.Type == "embedding" && ev.Status == "done" {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
}

func embeddingTasks(all []Task) []Task {
	out := []Task{}
	for _, t := range all {
		if t.Type == "embedding" {
			out = append(out, t)
		}
	}
	return out
}

// TestEmbedder_Run_DoesNotRetriggerOnSustainedReady:
// no repeated task publishing when ML stays ready with nothing to do.
func TestEmbedder_Run_DoesNotRetriggerOnSustainedReady(t *testing.T) {
	db := makeTestDB(t)
	// No asset missing a vector → Backfill should no-op, and repeated calls shouldn't spam tasks
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	e := NewEmbedder(db, &mockML{} /* ready */, nil, reg)
	e.SetPollInterval(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, embeddingTasks(emitted), "no repeated task publishing while ML stays ready with nothing to do")
}

// TestEmbedder_Run_CallsOnRecoveredAtChainTail asserts that the onRecovered
// hook is called at the tail of the recovery chain (after Backfill →
// reembed → BackfillOCR) on ML ready's false→true transition — service.go
// wires it to faces.RunPipeline, covering face-detection backlog
// accumulated while ML was down.
func TestEmbedder_Run_CallsOnRecoveredAtChainTail(t *testing.T) {
	db := makeTestDB(t)
	tog := &togglingML{}
	reg := NewTaskRegistry(func(Task) {})
	e := NewEmbedder(db, tog, nil, reg)
	e.SetPollInterval(50 * time.Millisecond)

	var calls atomic.Int32
	var lastCtx atomic.Bool // true once a non-nil ctx is received
	e.SetOnRecovered(func(ctx context.Context) {
		calls.Add(1)
		lastCtx.Store(ctx != nil)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	// Should not trigger while ready is still false.
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, calls.Load(), "onRecovered should not be called while ML isn't ready")

	tog.ready.Store(true)
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 5*time.Second, 50*time.Millisecond,
		"onRecovered should be called at the recovery chain tail after ML ready transitions")
	require.True(t, lastCtx.Load(), "onRecovered should receive a non-nil ctx")
}

// recordingOCRML records the input bytes received by the most recent OCR call; otherwise behaves like mockML.
type recordingOCRML struct {
	mockML
	lastOCRData []byte
}

func (m *recordingOCRML) OCR(data []byte) ([]mlclient.OCRLine, error) {
	m.lastOCRData = data
	return []mlclient.OCRLine{}, nil
}

// TestBackfillOCR_OversizedImageFallsBackToThumbnail: the OCR backfill (on
// the embedder side) is the third call site for the oversized-image guard —
// the image path reads the original directly, and an image over PIL's cap
// necessarily 500s, retrying on every ML recovery. After the guard kicks in,
// it must switch to feeding ML the large.jpg thumbnail bytes.
func TestBackfillOCR_OversizedImageFallsBackToThumbnail(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	thumbDir := t.TempDir()

	oversizedPath := filepath.Join(srcDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))
	assetID := insertAsset(t, db, oversizedPath, "indexed")

	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, assetID), 0o755))
	generatedThumb := makeTestJPEG(t, filepath.Join(thumbDir, assetID))
	largePath := filepath.Join(thumbDir, assetID, "large.jpg")
	require.NoError(t, os.Rename(generatedThumb, largePath))
	thumbBytes, err := os.ReadFile(largePath)
	require.NoError(t, err)

	ml := &recordingOCRML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))

	require.NoError(t, e.BackfillOCR(context.Background()))
	require.Equal(t, thumbBytes, ml.lastOCRData,
		"an oversized original should be swapped for the large.jpg thumbnail bytes fed to OCR, not the original's bytes")
}
