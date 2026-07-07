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
		r.failTask(taskID, started, "查询资产失败: "+err.Error())
		return
	}
	var targets []rebuildTarget
	for rows.Next() {
		var t rebuildTarget
		if err := rows.Scan(&t.id, &t.path); err != nil {
			rows.Close()
			r.failTask(taskID, started, "读取资产列表失败: "+err.Error())
			return
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.failTask(taskID, started, "读取资产列表失败: "+err.Error())
		return
	}

	// NOTE: we intentionally do NOT wipe clip_embeddings/asset_clip_idx here
	// anymore. Wiping upfront meant an asset whose source file is unreadable
	// at rebuild time (e.g. it lives on a removable drive that's currently
	// unplugged) would permanently lose its vector — ForceReprocess bails out
	// of processFileInternal at the very first os.ReadFile and never gets a
	// chance to re-embed it, so the asset would silently drop out of semantic
	// search forever (only "失败 N 张" in the task label hinted at it, with
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
			ID: taskID, Type: "rebuild", Label: "重建 AI 索引",
			Total: total, Current: cur, Progress: prog,
			Status: status, StartedAt: started,
		})
	}

	// finalPublish emits the terminal "done" task with optional failure count.
	finalPublish := func() {
		label := "重建 AI 索引"
		if f := atomic.LoadInt64(&failed); f > 0 {
			label = fmt.Sprintf("重建 AI 索引（失败 %d 张）", f)
		}
		r.reg.Upsert(Task{
			ID: taskID, Type: "rebuild", Label: label,
			Total: total, Current: atomic.LoadInt64(&processed), Progress: 1,
			Status: "done", StartedAt: started,
		})
	}

	// 人脸重聚类 + meta 写入，由空库和正常路径共用。
	finalize := func() {
		if r.ctx.Err() != nil {
			return // 服务关闭：不发 final，残留 running task 随服务消失
		}
		// 清理孤儿 CLIP 向量（asset 已不存在的 asset_clip_idx/clip_embeddings
		// 行）：承接以前全库清空里“清孤儿”的目的，见 run() 开头的说明。
		pruneOrphanClipVectors(r.db)
		// 人脸检测+重聚类（内部 CAS 防重入）。改调 RunPipeline 而非 RunClustering：
		// 下方 worker 循环删旧 face_detections 时已把对应资产的 face_scanned 置回
		// 0，必须靠 RunPipeline 的检测阶段重新扫一遍才能把脸补回来——RunClustering
		// 只会在既有 face_detections 上重新分组，不会触发检测，重建后会永久无脸。
		if err := r.faces.RunPipeline(r.ctx); err != nil {
			zap.L().Warn("rebuild: face pipeline failed", zap.Error(err))
		}
		// 换模型重聚类后，旧的空 persons（含用户命名）已无意义，清掉。
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

	// total=0：空库直接完成，跳过 worker pool，仍走 RunClustering + meta。
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

	// Worker pool sized by cfg.Workers — ML 调用是瓶颈，与索引器同档并发。
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
				// 旧人脸行先删（face_detections 是 INSERT 而非 upsert，
				// 不删会翻倍；face_person 经 FK CASCADE 一并清理）。
				// 人脸检测已移出 processFileInternal，ForceReprocess 不会再重新
				// 检测——必须把 face_scanned 置回 0，交给 finalize() 里的
				// RunPipeline 重新扫一遍，否则这批脸就永久空了。
				if _, err := r.db.Exec(`DELETE FROM face_detections WHERE asset_id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: clear faces failed", zap.String("asset", t.id), zap.Error(err))
				}
				if _, err := r.db.Exec(`UPDATE assets SET face_scanned=0 WHERE id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: reset face_scanned failed", zap.String("asset", t.id), zap.Error(err))
				}
				// 旧 CLIP 向量先删：见上方“不再全库清空”的说明 a)。
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

// modelGenStale 返回 photos_meta 里记录的模型代次是否落后于当前二进制。
// 键不存在(老库 / 首次启动)视为落后。
func modelGenStale(db *sql.DB) bool {
	var gen string
	_ = db.QueryRow(`SELECT value FROM photos_meta WHERE key=?`, mlModelGenKey).Scan(&gen)
	return gen != common.MLModelGen
}

// shouldStampModelGen 判断本轮 rebuild 是否可以盖章 ml_model_gen：
// total==0(空库，盖章合法)或至少有一个目标成功(failed<total)时可以盖章；
// total>0 且全部失败(典型场景是模型换代恰逢移动盘整体离线)时不能盖章，
// 否则 modelGenStale 判定永远不再触发，MaybeAutoRebuild 失去自动重试机会。
func shouldStampModelGen(total, failed int64) bool {
	return total == 0 || failed < total
}

// MaybeAutoRebuild 在模型代次变化时自动触发一次全量重建：等 ML 后端就绪
// (新模型缓存就位)后调 Start()。代次在 finalize() 成功后写入,所以重建
// 中途失败/断电会在下次启动重试。由 main.go 以 goroutine 启动。
func (r *Rebuilder) MaybeAutoRebuild(ready func() bool) {
	if !modelGenStale(r.db) {
		return
	}
	zap.L().Info("ML 模型代次变化，等待 ML 就绪后自动全量重建",
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
		zap.L().Warn("自动重建启动失败", zap.Error(err))
	}
}

// failTask publishes a terminal error state for the rebuild task and
// schedules its removal, mirroring the Embedder error convention.
func (r *Rebuilder) failTask(taskID string, started time.Time, msg string) {
	zap.L().Error("rebuild failed", zap.String("task", taskID), zap.String("reason", msg))
	r.reg.Upsert(Task{
		ID: taskID, Type: "rebuild", Label: "重建 AI 索引",
		Status: "error", Error: msg, StartedAt: started,
	})
	go func() {
		time.Sleep(taskCleanupDelay)
		r.reg.Remove(taskID)
	}()
}
