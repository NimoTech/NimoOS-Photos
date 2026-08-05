package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/aesthetic"
	"go.uber.org/zap"
)

const defaultEmbedderPollInterval = 30 * time.Second

// aestheticHeadVerKey is the key under which the aesthetic head version is
// recorded in photos_meta.
const aestheticHeadVerKey = "aesthetic_head_ver"

// EnsureAestheticHeadVer aligns the in-DB head version: on a mismatch, nulls
// out every score in the DB and stamps the new version in the same
// transaction. Unlike ml_model_gen's "stamp after success" pattern: nulling
// the scores atomically clears every old score with no dirty-data window, so
// stamping the version up front is safe; rescoring self-recovers via
// BackfillAesthetic's NULL query.
func EnsureAestheticHeadVer(db *sql.DB, ver string) error {
	var cur string
	_ = db.QueryRow(`SELECT value FROM photos_meta WHERE key=?`, aestheticHeadVerKey).Scan(&cur)
	if cur == ver {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE assets SET aesthetic_score=NULL`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO photos_meta(key, value) VALUES(?,?)
	    ON CONFLICT(key) DO UPDATE SET value=excluded.value`, aestheticHeadVerKey, ver); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ocrBackfillRetryDelay is the wait before retrying after the OCR backfill's
// "first pass failed entirely due to ML" case, giving an ML OCR model that
// hasn't warmed up yet right after a restart a bit of time, so a transient
// failure isn't surfaced to the user as a real one.
var ocrBackfillRetryDelay = 8 * time.Second

type Embedder struct {
	db           *sql.DB
	ml           MLProvider
	indexer      *Indexer
	reg          *TaskRegistry
	lastReady    atomic.Bool
	running      atomic.Bool
	ocrRunning   atomic.Bool
	pollInterval time.Duration

	// aestheticRunning / aestheticRerunPending / aestheticHead back
	// BackfillAesthetic's concurrency guard and rerun semantics, matching
	// ocrRunning/ocrRerunPending (see the field comments below).
	aestheticRunning      atomic.Bool
	aestheticRerunPending atomic.Bool
	aestheticHead         *aesthetic.Head

	// docVerdictRunning / docVerdictRerunPending guard BackfillDocVerdicts
	// the same way aestheticRunning/aestheticRerunPending guard
	// BackfillAesthetic. This one matters across chains, not just within
	// one: BackfillDocVerdicts is invoked from both the "ml-recovery" and
	// "post-batch-backfill" gated chains, which use distinct gate names and
	// so may legitimately fire concurrently — without this guard they'd
	// double-run the same query+compute loop.
	docVerdictRunning      atomic.Bool
	docVerdictRerunPending atomic.Bool

	// rerunPending / ocrRerunPending record "a trigger arrived while a
	// backfill pass was already running". A second call that loses the CAS
	// can't just silently return nil like before: the in-progress pass may
	// have already queried its target list before the assets the caller
	// just made backfillable existed (typically: MountGuard just marked
	// assets on a replugged drive back to offline=0) — swallowing the
	// trigger would mean that recovery never happens. Once set, a fresh
	// query and another pass run right after the current one ends.
	rerunPending    atomic.Bool
	ocrRerunPending atomic.Bool

	// onRecovered is called once at the tail of the ML-ready recovery chain
	// (after Backfill → reembed → BackfillOCR), covering the face-detection
	// backlog accumulated while ML was down. Injected as a function field
	// rather than directly importing FaceService (same pattern as
	// MountGuard's SetBackfill/SetBackfillOCR), avoiding a type coupling
	// between Embedder and FaceService; service.go wires it to
	// faces.RunPipeline. Safely skipped when nil (not wired up / tests).
	onRecovered func(context.Context)

	// gate throttles the ML-recovery chain (see runGated). Installed via
	// SetGate; nil in tests and any caller that never sets one, meaning no
	// throttling.
	gate *backfillGate
}

func NewEmbedder(db *sql.DB, ml MLProvider, idx *Indexer, reg *TaskRegistry) *Embedder {
	return &Embedder{
		db: db, ml: ml, indexer: idx, reg: reg,
		pollInterval: defaultEmbedderPollInterval,
	}
}

func (e *Embedder) SetPollInterval(d time.Duration) { e.pollInterval = d }

// SetGate installs the shared backfill throttle. A nil gate (tests, callers
// that never SetGate) means no throttling.
func (e *Embedder) SetGate(g *backfillGate) { e.gate = g }

func (e *Embedder) runGated(fn func()) {
	if e.gate == nil {
		fn()
		return
	}
	e.gate.Run("ml-recovery", fn)
}

// SetOnRecovered injects the callback invoked once at the tail of the ML-ready
// recovery chain (after Backfill/reembed/BackfillOCR), used to catch up on
// face detection backlog accumulated while ML was down.
func (e *Embedder) SetOnRecovered(fn func(context.Context)) { e.onRecovered = fn }

// clipTarget is one CLIP backfill candidate.
type clipTarget struct {
	id   string
	path string
}

// queryMissing lists the assets that are status='indexed' but have no row in
// asset_clip_idx, excluding any currently in cooldown after a prior failure.
func (e *Embedder) queryMissing(ctx context.Context, now time.Time) ([]clipTarget, error) {
	// a.offline=0: an asset on a currently-unplugged removable drive can't be
	// read, so retrying it here would just burn CPU on a guaranteed failure
	// every poll interval. MountGuard re-triggers Backfill right after the
	// drive is reinserted, so the gap is closed the moment the file is
	// reachable again.
	q := `
        SELECT a.id, a.file_path FROM assets a
        LEFT JOIN asset_clip_idx i ON i.asset_id = a.id
        WHERE a.status = 'indexed' AND a.offline = 0 AND i.asset_id IS NULL` +
		backfillCooldownSQL("a")
	rows, err := e.db.QueryContext(ctx, q, string(backfillCLIP), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clipTarget
	for rows.Next() {
		var tg clipTarget
		if err := rows.Scan(&tg.id, &tg.path); err != nil {
			return nil, err
		}
		out = append(out, tg)
	}
	return out, rows.Err()
}

// hasEmbeddingForPath looks up whether the given path already has a row in
// asset_clip_idx. Looked up by path rather than asset_id because Backfill's
// main loop only holds a list of paths.
func (e *Embedder) hasEmbeddingForPath(path string) bool {
	var n int
	_ = e.db.QueryRow(`
        SELECT 1 FROM asset_clip_idx i
        JOIN assets a ON a.id = i.asset_id
        WHERE a.file_path = ? LIMIT 1`, path).Scan(&n)
	return n == 1
}

// Backfill reruns ML for every asset that's status='indexed' but missing a
// CLIP vector. Safe for concurrent calls: a second call returns nil
// immediately but sets rerunPending, so the in-progress pass automatically
// runs one more round (requerying targets) once it finishes, guaranteeing
// the trigger isn't swallowed.
//
// Known narrow window: if the flag is set right after the current pass's
// last pending check but before running is released, that trigger won't be
// consumed until the next Backfill call (an ML-ready transition / mount
// recovery / manual trigger). The window is extremely narrow and these
// trigger sources all recur periodically, so no double-check is done.
func (e *Embedder) Backfill(ctx context.Context) error {
	if !e.running.CompareAndSwap(false, true) {
		e.rerunPending.Store(true)
		return nil
	}
	defer e.running.Store(false)

	for {
		if err := e.backfillOnce(ctx); err != nil {
			return err
		}
		if !e.rerunPending.CompareAndSwap(true, false) {
			return nil
		}
	}
}

// embedOne fills one asset's CLIP vector, preferring the thumbnail path.
// When the thumb is present the original file is never opened; the full
// ForceReprocess pipeline is only the fallback for assets without a thumb.
// A light-path ML error does not fall through to ForceReprocess — that
// would re-issue the same doomed ML call after reading gigabytes more.
func (e *Embedder) embedOne(tg clipTarget) bool {
	if hasSmallThumb(e.indexer.thumbDir, tg.id) {
		_ = e.indexer.embedClip(tg.id, nil)
		return e.hasEmbeddingForPath(tg.path)
	}
	e.indexer.ForceReprocess(tg.path, processOpts{force: true, skipExif: true, skipThumb: true})
	return e.hasEmbeddingForPath(tg.path)
}

// backfillOnce is Backfill's single-pass body (query targets + backfill each
// one + report task progress), without the concurrency guard or rerun loop.
func (e *Embedder) backfillOnce(ctx context.Context) error {
	targets, err := e.queryMissing(ctx, time.Now())
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	taskID := fmt.Sprintf("embedding_%d", time.Now().UnixNano())
	started := time.Now()
	total := int64(len(targets))
	pubRunning := func(processed int64) {
		e.reg.Upsert(Task{
			ID:        taskID,
			Type:      "embedding",
			Label:     "Generating AI index",
			Total:     total,
			Current:   processed,
			Progress:  float64(processed) / float64(total),
			Status:    "running",
			StartedAt: started,
		})
	}
	pubRunning(0)

	var processed, success, failed int64
	type failedTarget struct {
		tg clipTarget
	}
	var failures []failedTarget
	for _, tg := range targets {
		if ctx.Err() != nil {
			break
		}
		if e.embedOne(tg) {
			success++
			clearBackfillFailure(e.db, backfillCLIP, tg.id)
		} else {
			failed++
			failures = append(failures, failedTarget{tg: tg})
		}
		processed++
		pubRunning(processed)
	}
	// Environmental guard: an all-failed pass with ML actually unreachable
	// (IsReady()==false) means the ML backend itself is down (same condition
	// as TaskErrMLLostDuringBackfill below) — recording per-asset failures
	// would walk healthy assets up the cooldown ladder. But when ML is
	// ready, an all-failed pass means the assets themselves are broken and
	// MUST be recorded, or that corrupt set gets re-read every gate window
	// forever and never converges.
	if success > 0 || failed == 0 || e.ml.IsReady() {
		now := time.Now()
		for _, f := range failures {
			recordBackfillFailure(e.db, backfillCLIP, f.tg.id, now,
				fmt.Errorf("clip backfill produced no embedding"))
		}
	}

	// If ctx is already cancelled (user cancellation or service shutdown),
	// don't publish the final task — partial completion isn't completion,
	// and the leftover running task in the registry naturally disappears
	// when the service shuts down.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	final := Task{
		ID:        taskID,
		Type:      "embedding",
		Label:     "Generating AI index",
		Total:     total,
		Current:   success,
		StartedAt: started,
	}
	if success == 0 && failed > 0 {
		final.Status = "error"
		final.SetError(TaskErrMLLostDuringBackfill, nil)
	} else {
		final.Status = "done"
		final.Progress = 1
		if failed > 0 {
			final.Label = fmt.Sprintf("Generating AI index (%d failed)", failed)
		}
	}
	e.reg.Upsert(final)
	go func() {
		time.Sleep(6 * time.Second)
		if e.reg != nil {
			e.reg.Remove(taskID)
		}
	}()
	return nil
}

// queryMissingOCR lists assets that are status='indexed' but have
// incomplete OCR data: either asset_ocr has no row (OCR never ran — even
// when no text is recognized a row with an empty text is written), or
// coverage is NULL (an older OCR run didn't store the text-box area and
// needs a rerun to fill it in), or boxes_ver=0 (an older run didn't store
// per-line coordinates into asset_ocr_lines, needs a rerun for search-hit
// highlighting).
func (e *Embedder) queryMissingOCR(ctx context.Context, now time.Time) ([]ocrTarget, error) {
	// a.offline=0: same reasoning as queryMissing above — skip assets whose
	// source is unreachable because their removable drive is unplugged.
	q := `
        SELECT a.id, a.file_path, COALESCE(a.mime_type,'') LIKE 'video/%'
        FROM assets a
        LEFT JOIN asset_ocr o ON o.asset_id = a.id
        WHERE a.status = 'indexed' AND a.deleted_at IS NULL AND a.offline = 0
          AND COALESCE(a.mime_type,'') NOT LIKE 'video/%'
          AND (o.asset_id IS NULL OR o.coverage IS NULL OR COALESCE(o.boxes_ver,0) = 0)` +
		backfillCooldownSQL("a")
	rows, err := e.db.QueryContext(ctx, q, string(backfillOCR), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ocrTarget
	for rows.Next() {
		var t ocrTarget
		if err := rows.Scan(&t.id, &t.path, &t.isVideo); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type ocrTarget struct {
	id      string
	path    string
	isVideo bool
}

// BackfillOCR reruns ML OCR for every asset missing OCR text.
// Images read the original (small text on receipts/documents is lost at
// thumbnail resolution); videos have no ready-made keyframe file, so they
// fall back to the large.jpg (1280px) thumbnail.
// Safe for concurrent calls: a second call returns nil immediately but sets
// ocrRerunPending, so the in-progress pass automatically runs one more round
// once it finishes — same reasoning and window as Backfill.
func (e *Embedder) BackfillOCR(ctx context.Context) error {
	if !e.ocrRunning.CompareAndSwap(false, true) {
		e.ocrRerunPending.Store(true)
		return nil
	}
	defer e.ocrRunning.Store(false)

	for {
		if err := e.backfillOCROnce(ctx); err != nil {
			return err
		}
		if !e.ocrRerunPending.CompareAndSwap(true, false) {
			return nil
		}
	}
}

// backfillOCROnce is BackfillOCR's single-pass body, without the concurrency
// guard or rerun loop.
func (e *Embedder) backfillOCROnce(ctx context.Context) error {
	targets, err := e.queryMissingOCR(ctx, time.Now())
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	taskID := fmt.Sprintf("ocr_%d", time.Now().UnixNano())
	started := time.Now()
	total := int64(len(targets))
	pubRunning := func(processed int64) {
		e.reg.Upsert(Task{
			ID:        taskID,
			Type:      "ocr",
			Label:     "Recognizing text in images",
			Total:     total,
			Current:   processed,
			Progress:  float64(processed) / float64(total),
			Status:    "running",
			StartedAt: started,
		})
	}
	pubRunning(0)

	// passFailures/passSuccess are captured by pass below and reset at the
	// start of every call, so only the LAST call's results (the built-in
	// ML-cold-start retry re-runs pass once) ever reach the ledger
	// write-back after this function's final pass — see the retry block and
	// the write-back below.
	var passFailures map[string]error
	var passSuccess []string

	// pass runs through the given targets once, returning
	// processed/failed counts (readFail = source file unreadable, ocrFail =
	// ML call failed).
	pass := func(ts []ocrTarget) (processed, failed, readFail, ocrFail int64) {
		passFailures = map[string]error{}
		passSuccess = nil
		for _, t := range ts {
			if ctx.Err() != nil {
				break
			}
			src := t.path
			if t.isVideo {
				src = filepath.Join(e.indexer.thumbDir, t.id, "large.jpg")
			}
			data, rerr := os.ReadFile(src)
			if rerr != nil || len(data) == 0 {
				// The original/thumbnail is unreadable (file deleted,
				// thumbnail missing, etc.), unrelated to ML. Recorded in the
				// ledger too: a corrupt/missing source is exactly what the
				// cooldown exists to stop re-reading every pass.
				readFail++
				failed++
				processed++
				cause := rerr
				if cause == nil {
					cause = fmt.Errorf("source file empty: %s", src)
				}
				passFailures[t.id] = cause
				pubRunning(processed)
				continue
			}
			// Oversized-original guard (same one as
			// detectFaceScanTarget / inline indexing OCR): an image over
			// PIL's limit necessarily 500s on the original and would retry
			// on every recovery chain pass, so fall back to the thumbnail.
			if !t.isVideo && oversizedForML(data) {
				if thumb := readLargeOrSmallThumb(e.indexer.thumbDir, t.id); len(thumb) > 0 {
					data = thumb
				} else {
					zap.L().Warn("OCR backfill: oversized image with no thumbnail fallback available, skipping",
						zap.String("asset_id", t.id), zap.String("path", t.path))
					readFail++
					failed++
					processed++
					passFailures[t.id] = fmt.Errorf("oversized image with no thumbnail fallback available")
					pubRunning(processed)
					continue
				}
			}
			if oerr := e.indexer.ocrAsset(t.id, data); oerr != nil {
				ocrFail++
				failed++
				passFailures[t.id] = oerr
			} else {
				if derr := e.indexer.computeDocVerdict(t.id); derr != nil {
					zap.L().Warn("doc verdict after ocr backfill failed", zap.String("asset_id", t.id), zap.Error(derr))
				}
				passSuccess = append(passSuccess, t.id)
			}
			processed++
			pubRunning(processed)
		}
		return
	}

	processed, failed, readFail, ocrFail := pass(targets)

	// The first pass failed entirely due to ML (likely the OCR model
	// hasn't warmed up right after a restart): wait a bit and retry the
	// targets still missing OCR once, only reporting an error if that
	// still fails too. Avoids surfacing a false failure on every
	// redeployment that's really just "ML not warmed up yet".
	if processed > 0 && failed == processed && ocrFail > 0 && ctx.Err() == nil {
		zap.L().Info("OCR backfill's first pass failed entirely (ML likely not ready), retrying once shortly",
			zap.Int64("ml_fail", ocrFail))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ocrBackfillRetryDelay):
		}
		if ts2, qerr := e.queryMissingOCR(ctx, time.Now()); qerr == nil && len(ts2) > 0 {
			total = int64(len(ts2))
			pubRunning(0)
			processed, failed, readFail, ocrFail = pass(ts2)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Ledger write-back from the FINAL pass only; skip entirely when the
	// pass is classified as ML-down (all processed failed via ocrFail AND
	// ML is actually unreachable). When ML is ready, an all-failed pass
	// means the assets themselves are broken and MUST be recorded, or that
	// corrupt set gets re-read every gate window forever and never
	// converges.
	if !(processed > 0 && failed == processed && ocrFail > 0 && !e.ml.IsReady()) {
		now := time.Now()
		for id, cause := range passFailures {
			recordBackfillFailure(e.db, backfillOCR, id, now, cause)
		}
	}
	for _, id := range passSuccess {
		clearBackfillFailure(e.db, backfillOCR, id)
	}

	final := Task{
		ID:        taskID,
		Type:      "ocr",
		Label:     "Recognizing text in images",
		Total:     total,
		Current:   processed - failed,
		StartedAt: started,
	}
	if processed > 0 && failed == processed {
		// Only report error when everything failed, with the accurate
		// reason given for the real cause (previously always writing
		// "ML disconnected" was a misdiagnosis).
		final.Status = "error"
		switch {
		case ocrFail == 0 && readFail > 0:
			final.SetError(TaskErrOCRSourceReadFailed, map[string]string{"readFail": strconv.FormatInt(readFail, 10)})
		case ocrFail > 0 && readFail == 0:
			final.SetError(TaskErrMLLostDuringBackfill, nil)
		default:
			final.SetError(TaskErrOCRBackfillFailed, map[string]string{
				"readFail": strconv.FormatInt(readFail, 10),
				"ocrFail":  strconv.FormatInt(ocrFail, 10),
			})
		}
	} else {
		final.Status = "done"
		final.Progress = 1
		if failed > 0 {
			final.Label = fmt.Sprintf("Recognizing text in images (%d failed)", failed)
		}
	}
	if failed > 0 {
		zap.L().Warn("OCR backfill had failures",
			zap.Int64("total", total), zap.Int64("processed", processed),
			zap.Int64("read_fail", readFail), zap.Int64("ml_fail", ocrFail),
			zap.String("status", final.Status))
	}
	e.reg.Upsert(final)
	go func() {
		time.Sleep(6 * time.Second)
		if e.reg != nil {
			e.reg.Remove(taskID)
		}
	}()
	return nil
}

// BackfillDocVerdicts computes the doc-classification mixed-criteria verdict
// for assets whose OCR line coordinates are already stored (boxes_ver=1), doc
// verdict hasn't been computed yet (doc_ver=0), and image vector is ready.
// Pure local math (at most one text embedding for the prompt vectors),
// milliseconds per asset, so it isn't hooked up to TaskRegistry and doesn't
// publish task events (unlike BackfillOCR, which runs inference). Assets
// without a vector aren't selected; they converge on the next recovery-chain
// hook after CLIP Backfill fills in their vector.
//
// Safe for concurrent calls: unlike Backfill/BackfillOCR/BackfillAesthetic,
// each of which is only ever driven by a single chain, this is invoked from
// both the "ml-recovery" and "post-batch-backfill" gated chains — distinct
// gate names that may legitimately fire at the same time — so it needs the
// same guard even though nothing else about it changed. A second call
// returns nil immediately but sets docVerdictRerunPending, so the
// in-progress pass automatically runs one more round (requerying targets)
// once it finishes, guaranteeing the trigger isn't swallowed (same
// mechanism and narrow-window caveat as Backfill's rerunPending; see its
// doc comment).
func (e *Embedder) BackfillDocVerdicts(ctx context.Context) error {
	if !e.docVerdictRunning.CompareAndSwap(false, true) {
		e.docVerdictRerunPending.Store(true)
		return nil
	}
	defer e.docVerdictRunning.Store(false)

	for {
		if err := e.backfillDocVerdictsOnce(ctx); err != nil {
			return err
		}
		if !e.docVerdictRerunPending.CompareAndSwap(true, false) {
			return nil
		}
	}
}

// backfillDocVerdictsOnce is BackfillDocVerdicts's single-pass body (query
// targets + compute each one's verdict), without the concurrency guard or
// rerun loop.
func (e *Embedder) backfillDocVerdictsOnce(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `
        SELECT o.asset_id
        FROM asset_ocr o
        JOIN assets a ON a.id = o.asset_id
        JOIN asset_clip_idx ci ON ci.asset_id = o.asset_id
        WHERE o.doc_ver = 0 AND o.boxes_ver = 1
          AND a.status = 'indexed' AND a.deleted_at IS NULL AND a.offline = 0`)
	if err != nil {
		return err
	}
	ids := make([]string, 0, 32)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := e.indexer.computeDocVerdict(id); err != nil {
			zap.L().Warn("doc verdict backfill failed", zap.String("asset_id", id), zap.Error(err))
		}
	}
	return nil
}

// SetAestheticHead injects the aesthetic-scoring head; BackfillAesthetic is a no-op when nil.
func (e *Embedder) SetAestheticHead(h *aesthetic.Head) { e.aestheticHead = h }

// BackfillAesthetic computes the aesthetic score for assets that have a
// CLIP vector but aesthetic_score IS NULL. Pure local matrix multiply,
// doesn't depend on ML being online; doesn't filter out offline assets
// (scoring only reads the in-DB vector, never touches the file). Concurrency
// guard and rerun semantics match BackfillOCR.
func (e *Embedder) BackfillAesthetic(ctx context.Context) error {
	if e.aestheticHead == nil {
		return nil
	}
	if !e.aestheticRunning.CompareAndSwap(false, true) {
		e.aestheticRerunPending.Store(true)
		return nil
	}
	defer e.aestheticRunning.Store(false)
	for {
		if err := e.backfillAestheticOnce(ctx); err != nil {
			return err
		}
		if !e.aestheticRerunPending.CompareAndSwap(true, false) {
			return nil
		}
	}
}

// backfillAestheticOnce is BackfillAesthetic's single-pass body, without the
// concurrency guard or rerun loop.
func (e *Embedder) backfillAestheticOnce(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.id FROM assets a
        JOIN asset_clip_idx ci ON ci.asset_id = a.id
        WHERE a.aesthetic_score IS NULL AND a.deleted_at IS NULL AND a.status = 'indexed'`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	taskID := fmt.Sprintf("aesthetic_%d", time.Now().UnixNano())
	started := time.Now()
	total := int64(len(ids))
	pubRunning := func(processed int64) {
		e.reg.Upsert(Task{
			ID: taskID, Type: "aesthetic", Label: "Scoring photo aesthetics",
			Total: total, Current: processed,
			Progress: float64(processed) / float64(total),
			Status:   "running", StartedAt: started,
		})
	}
	pubRunning(0)

	var processed, failed int64
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		vec := readClipVector(e.db, id)
		if vec == nil {
			// The vector was deleted between the query and the read (e.g. a
			// rebuild race): skip it, leaving it NULL to converge next round.
			processed++
			failed++
			pubRunning(processed)
			continue
		}
		s := e.aestheticHead.Score(vec)
		if math.IsNaN(s) || math.IsInf(s, 0) {
			processed++
			failed++
			pubRunning(processed)
			continue
		}
		if _, err := e.db.Exec(`UPDATE assets SET aesthetic_score=? WHERE id=?`, s, id); err != nil {
			zap.L().Warn("aesthetic backfill: failed to write score", zap.String("asset_id", id), zap.Error(err))
			failed++
		}
		processed++
		pubRunning(processed)
	}

	final := Task{
		ID: taskID, Type: "aesthetic", Label: "Scoring photo aesthetics",
		Total: total, Current: processed - failed,
		Status: "done", Progress: 1, StartedAt: started,
	}
	e.reg.Upsert(final)
	go func() {
		time.Sleep(6 * time.Second)
		if e.reg != nil {
			e.reg.Remove(taskID)
		}
	}()
	return nil
}

// Run is the main loop: checks ML ready state every pollInterval,
// triggering a Backfill (executed asynchronously in a goroutine) when it
// detects a false→true transition.
// If ML is already ready when the service starts, the first tick's
// prev=false also triggers it — matching spec §5.2.
func (e *Embedder) Run(ctx context.Context) {
	interval := e.pollInterval
	if interval <= 0 {
		interval = defaultEmbedderPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	e.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick detects an ML-ready transition and, when one occurs, asynchronously triggers Backfill.
func (e *Embedder) tick(ctx context.Context) {
	ready := e.ml.IsReady()
	prev := e.lastReady.Swap(ready)
	if ready && !prev {
		go e.runGated(func() {
			// Backfill first (fills assets that never got an embedding), then the
			// one-time re-embed of all existing assets from their thumbnails,
			// then OCR for assets indexed before OCR support existed, then doc
			// verdicts for OCR'd assets missing the mixed-criteria judgment
			// (BackfillDocVerdicts), then aesthetic scores for assets whose CLIP
			// vector arrived while ML was down (BackfillAesthetic, a pure local
			// computation that doesn't depend on ML, but is still hung on this
			// same recovery chain to converge along with it), and finally faces
			// (RunPipeline, via onRecovered) — covers detection backlog
			// accumulated while ML was down.
			_ = e.Backfill(ctx)
			e.reembedThumbnailsOnce()
			_ = e.BackfillOCR(ctx)
			_ = e.BackfillDocVerdicts(ctx)
			_ = e.BackfillAesthetic(ctx)
			if e.onRecovered != nil {
				e.onRecovered(ctx)
			}
		})
	}
}

// reembedThumbnailsOnce performs a one-time CLIP re-embed of every existing asset
// from its displayed thumbnail, the first time the service comes up after the
// switch to thumbnail-based embeddings. Previously the embedding was computed on
// the full-resolution source, which diverged from the shown frame (especially for
// high-detail video keyframes) and let irrelevant videos outrank better photo
// matches. A marker file in the data dir ensures this runs exactly once; delete it
// to force a re-run (e.g. after a model change).
func (e *Embedder) reembedThumbnailsOnce() {
	if e.indexer == nil {
		return
	}
	marker := filepath.Join(filepath.Dir(e.indexer.thumbDir), ".clip_reembed_thumb_v1.done")
	if _, err := os.Stat(marker); err == nil {
		return // already done
	}
	ok, failed := e.indexer.ReembedAllClip()
	zap.L().Info("one-time CLIP re-embed from thumbnails complete",
		zap.Int("reembedded", ok), zap.Int("failed", failed))
	if failed == 0 {
		if err := os.WriteFile(marker, []byte(fmt.Sprintf("reembedded=%d\n", ok)), 0o644); err != nil {
			zap.L().Warn("failed to write re-embed marker", zap.Error(err))
		}
	}
}
