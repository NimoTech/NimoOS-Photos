package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"go.uber.org/zap"
)

// ErrRebuildRunning is returned by Start when a rebuild is already in progress.
var ErrRebuildRunning = errors.New("rebuild already running")

// Rebuilder re-runs the AI pipeline (CLIP / faces / OCR, honouring the feature
// flags) over every indexed asset, then re-clusters faces. Progress is
// published through the shared TaskRegistry, so the frontend reuses the
// existing Socket.IO task stream.
type Rebuilder struct {
	ctx     context.Context // service parent ctx — NOT the HTTP request ctx
	db      *sql.DB
	indexer *Indexer
	faces   *FaceService
	reg     *TaskRegistry
	workers int
	running atomic.Bool
}

func NewRebuilder(ctx context.Context, db *sql.DB, idx *Indexer, faces *FaceService, reg *TaskRegistry, workers int) *Rebuilder {
	if workers < 1 {
		workers = 1
	}
	return &Rebuilder{ctx: ctx, db: db, indexer: idx, faces: faces, reg: reg, workers: workers}
}

// Start launches the rebuild in a background goroutine and returns its task ID.
// The goroutine is bound to the service parent ctx so it survives the HTTP
// request that triggered it.
func (r *Rebuilder) Start() (string, error) {
	if !r.running.CompareAndSwap(false, true) {
		return "", ErrRebuildRunning
	}
	taskID := fmt.Sprintf("rebuild_%d", time.Now().UnixNano())
	go func() {
		defer r.running.Store(false)
		r.run(taskID)
	}()
	return taskID, nil
}

type rebuildTarget struct {
	id   string
	path string
}

func (r *Rebuilder) run(taskID string) {
	started := time.Now()

	// offline=0: exclude assets currently living on an unplugged removable
	// drive. Their source file can't be read anyway (every worker would just
	// count them as a Stat failure below), and once the drive comes back
	// MountGuard's post-remount Backfill/BackfillOCR heals any CLIP/OCR gap
	// left over from a model-generation rebuild that ran while they were away.
	rows, err := r.db.Query(`SELECT id, file_path FROM assets WHERE status='indexed' AND deleted_at IS NULL AND offline=0`)
	if err != nil {
		r.failTask(taskID, started, TaskErrQueryAssetsFailed, map[string]string{"detail": err.Error()})
		return
	}
	var targets []rebuildTarget
	for rows.Next() {
		var t rebuildTarget
		if err := rows.Scan(&t.id, &t.path); err != nil {
			rows.Close()
			r.failTask(taskID, started, TaskErrReadAssetListFailed, map[string]string{"detail": err.Error()})
			return
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.failTask(taskID, started, TaskErrReadAssetListFailed, map[string]string{"detail": err.Error()})
		return
	}

	// NOTE: we intentionally do NOT wipe clip_embeddings/asset_clip_idx here
	// anymore. Wiping upfront meant an asset whose source file is unreadable
	// at rebuild time (e.g. it lives on a removable drive that's currently
	// unplugged) would permanently lose its vector — ForceReprocess bails out
	// of processFileInternal at the very first os.ReadFile and never gets a
	// chance to re-embed it, so the asset would silently drop out of semantic
	// search forever (only "N failed" in the task label hinted at it, with
	// no way to recover). The old goals of that wipe are now handled without
	// destroying data that can't be recomputed:
	//   - orphan vectors (assets that no longer exist) are swept by
	//     pruneOrphanClipVectors in finalize(), below;
	//   - stale/duplicate vectors for assets that DO get reprocessed are
	//     handled per-asset in the worker loop via dropClipVector, right
	//     before ForceReprocess — and writeClipEmbedding's UPDATE-then-INSERT
	//     upsert (indexer.go) is safe either way since asset_clip_idx has a
	//     UNIQUE(asset_id) constraint, so there's never more than one vector
	//     row per asset.
	// Semantics this creates: within the same ML model generation, a manual
	// rebuild over an asset whose file is temporarily unreadable keeps its
	// existing faces/vector (still searchable with old-but-valid data) and
	// just counts it as failed for retry next time. This does NOT weaken the
	// model-upgrade path: when the CLIP dimension changes, migrateClipDim
	// already DROPs the whole clip_embeddings table at startup, before any
	// rebuild worker runs.

	total := int64(len(targets))
	var processed, failed int64

	publish := func(status string) {
		cur := atomic.LoadInt64(&processed)
		prog := 1.0
		if total > 0 {
			prog = float64(cur) / float64(total)
		}
		r.reg.Upsert(Task{
			ID: taskID, Type: "rebuild", Label: "Rebuilding AI index",
			Total: total, Current: cur, Progress: prog,
			Status: status, StartedAt: started,
		})
	}

	// finalPublish emits the terminal "done" task with optional failure count.
	finalPublish := func() {
		label := "Rebuilding AI index"
		if f := atomic.LoadInt64(&failed); f > 0 {
			label = fmt.Sprintf("Rebuilding AI index (%d failed)", f)
		}
		r.reg.Upsert(Task{
			ID: taskID, Type: "rebuild", Label: label,
			Total: total, Current: atomic.LoadInt64(&processed), Progress: 1,
			Status: "done", StartedAt: started,
		})
	}

	// Face re-clustering + meta write, shared by both the empty-DB and normal paths.
	finalize := func() {
		if r.ctx.Err() != nil {
			return // service shutting down: don't publish final, the leftover running task disappears with the service
		}
		// Clean up orphaned CLIP vectors (asset_clip_idx/clip_embeddings
		// rows whose asset no longer exists): carries forward the "clean
		// up orphans" purpose of the old full-DB wipe, see the note at the
		// top of run().
		pruneOrphanClipVectors(r.db)
		// Face detection + re-clustering (internally CAS-guarded against
		// re-entrancy). Calls RunPipeline rather than RunClustering: the
		// worker loop below has already reset face_scanned to 0 for assets
		// whose old face_detections it deleted — only RunPipeline's
		// detection stage rescanning them can bring the faces back;
		// RunClustering only regroups existing face_detections without
		// triggering detection, which would leave the rebuild's assets
		// permanently faceless.
		if err := r.faces.RunPipeline(r.ctx); err != nil {
			zap.L().Warn("rebuild: face pipeline failed", zap.Error(err))
		}
		// After re-clustering under a new model, old empty persons
		// (including user-named ones) are meaningless — clean them up.
		if _, err := r.db.Exec(`DELETE FROM persons WHERE id NOT IN
			(SELECT person_id FROM face_person WHERE person_id IS NOT NULL)`); err != nil {
			zap.L().Warn("rebuild: prune empty persons failed", zap.Error(err))
		}
		if _, err := r.db.Exec(`INSERT INTO photos_meta(key,value) VALUES('index_last_rebuilt',?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			zap.L().Warn("rebuild: write meta failed", zap.Error(err))
		}
		if shouldStampModelGen(total, atomic.LoadInt64(&failed)) {
			if _, err := r.db.Exec(`INSERT INTO photos_meta(key,value) VALUES(?,?)
				ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				mlModelGenKey, common.MLModelGen); err != nil {
				zap.L().Warn("rebuild: write ml_model_gen failed", zap.Error(err))
			}
		} else {
			zap.L().Warn("rebuild: all targets failed — leaving ml_model_gen stale so next boot retries",
				zap.Int("targets", len(targets)))
		}
	}

	// total=0: an empty DB completes immediately, skipping the worker pool, but still goes through finalize()'s RunPipeline + meta.
	if total == 0 {
		finalize()
		if r.ctx.Err() != nil {
			return
		}
		finalPublish()
		go func() {
			time.Sleep(taskCleanupDelay)
			r.reg.Remove(taskID)
		}()
		return
	}

	publish("running")

	// Worker pool sized by cfg.Workers — ML calls are the bottleneck, matching the indexer's concurrency level.
	queue := make(chan rebuildTarget)
	var wg sync.WaitGroup
	for i := 0; i < r.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range queue {
				if r.ctx.Err() != nil {
					continue // drain
				}
				// Guard: only destroy the existing face/vector rows once we know
				// the source file can actually be read back — otherwise
				// ForceReprocess fails at its first os.ReadFile and we'd be left
				// with neither the old data nor new data (permanent, silent
				// loss). os.Stat instead of a full ReadFile is enough here; the
				// remaining stat→read TOCTOU window (file vanishes in between)
				// still loses that ONE asset's old data — the guard narrows the
				// blast radius from "every asset processed after unplug" down to
				// a single asset in a millisecond window, it does not eliminate
				// it. Fully closing it would require processFileInternal to
				// accept pre-read bytes; not worth the churn.
				if _, err := os.Stat(t.path); err != nil {
					zap.L().Warn("rebuild: source file unreadable, keeping existing ML data",
						zap.String("path", t.path), zap.Error(err))
					atomic.AddInt64(&processed, 1)
					atomic.AddInt64(&failed, 1)
					publish("running")
					continue
				}
				// Delete old face rows first (face_detections is an
				// INSERT, not an upsert — not deleting them first would
				// double them up; face_person is cleaned up together via
				// FK CASCADE). Face detection has been removed from
				// processFileInternal, so ForceReprocess won't re-detect
				// on its own — face_scanned must be reset to 0, handing it
				// off to finalize()'s RunPipeline to rescan, or this batch
				// of faces would be permanently empty.
				if _, err := r.db.Exec(`DELETE FROM face_detections WHERE asset_id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: clear faces failed", zap.String("asset", t.id), zap.Error(err))
				}
				if _, err := r.db.Exec(`UPDATE assets SET face_scanned=0 WHERE id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: reset face_scanned failed", zap.String("asset", t.id), zap.Error(err))
				}
				// The old aesthetic score is based on the old model's
				// vector, so clear it too; it's automatically restored by
				// inline scoring (at writeClipEmbedding's exit point) when
				// ForceReprocess rewrites the vector, no separate task needed.
				if _, err := r.db.Exec(`UPDATE assets SET aesthetic_score=NULL WHERE id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: reset aesthetic_score failed", zap.String("asset", t.id), zap.Error(err))
				}
				// Delete the old CLIP vector first: see the "no longer wiping the whole DB" note above.
				dropClipVector(r.db, t.id)
				ok := r.indexer.ForceReprocess(t.path, processOpts{force: true, skipExif: true, skipThumb: true})
				atomic.AddInt64(&processed, 1)
				if !ok {
					atomic.AddInt64(&failed, 1)
				}
				publish("running")
			}
		}()
	}
	for _, t := range targets {
		queue <- t
	}
	close(queue)
	wg.Wait()

	finalize()
	if r.ctx.Err() != nil {
		return
	}

	finalPublish()
	go func() {
		time.Sleep(taskCleanupDelay)
		r.reg.Remove(taskID)
	}()
}

const mlModelGenKey = "ml_model_gen"

// modelGenStale returns whether the model generation recorded in
// photos_meta is behind the current binary. A missing key (an old DB /
// first startup) is treated as stale.
func modelGenStale(db *sql.DB) bool {
	var gen string
	_ = db.QueryRow(`SELECT value FROM photos_meta WHERE key=?`, mlModelGenKey).Scan(&gen)
	return gen != common.MLModelGen
}

// shouldStampModelGen decides whether this rebuild pass may stamp
// ml_model_gen: it's allowed when total==0 (an empty DB, stamping is
// legitimate) or when at least one target succeeded (failed<total); it's
// not allowed when total>0 and everything failed (the typical scenario is
// a model upgrade coinciding with every removable drive being offline),
// otherwise modelGenStale would never trigger again and MaybeAutoRebuild
// would lose its chance to auto-retry.
func shouldStampModelGen(total, failed int64) bool {
	return total == 0 || failed < total
}

// MaybeAutoRebuild automatically triggers one full rebuild when the model
// generation changes: calls Start() once the ML backend is ready (the new
// model cache is in place). The generation is written after finalize()
// succeeds, so a rebuild that fails or loses power midway is retried on the
// next startup. Launched by main.go as a goroutine.
func (r *Rebuilder) MaybeAutoRebuild(ready func() bool) {
	if !modelGenStale(r.db) {
		return
	}
	zap.L().Info("ML model generation changed, waiting for ML to be ready before auto full rebuild",
		zap.String("target_gen", common.MLModelGen))
	for r.ctx.Err() == nil && !ready() {
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	if r.ctx.Err() != nil {
		return
	}
	if _, err := r.Start(); err != nil && err != ErrRebuildRunning {
		zap.L().Warn("failed to start auto rebuild", zap.Error(err))
	}
}

// failTask publishes a terminal error state for the rebuild task and
// schedules its removal, mirroring the Embedder error convention.
// errKey/errParams are the structured i18n error (see Task.SetError); msg is for logging only.
func (r *Rebuilder) failTask(taskID string, started time.Time, errKey string, errParams map[string]string) {
	t := Task{
		ID: taskID, Type: "rebuild", Label: "Rebuilding AI index",
		Status: "error", StartedAt: started,
	}
	t.SetError(errKey, errParams)
	zap.L().Error("rebuild failed", zap.String("task", taskID), zap.String("reason", t.Error))
	r.reg.Upsert(t)
	go func() {
		time.Sleep(taskCleanupDelay)
		r.reg.Remove(taskID)
	}()
}
