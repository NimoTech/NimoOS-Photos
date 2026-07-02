package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	rows, err := r.db.Query(`SELECT id, file_path FROM assets WHERE status='indexed' AND deleted_at IS NULL`)
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

	// Wipe the CLIP vector index so the rebuild starts from a clean slate: this
	// drops orphan vectors left behind by previously deleted assets (vec0 rows
	// the FK cascade can't reach) and guarantees no stale/duplicate embeddings
	// survive. writeClipEmbedding re-creates asset_clip_idx rows and vectors as
	// it re-embeds each asset below.
	if _, err := r.db.Exec(`DELETE FROM clip_embeddings`); err != nil {
		zap.L().Warn("rebuild: clear clip_embeddings failed", zap.Error(err))
	}
	if _, err := r.db.Exec(`DELETE FROM asset_clip_idx`); err != nil {
		zap.L().Warn("rebuild: clear asset_clip_idx failed", zap.Error(err))
	}

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
		// 人脸重聚类（内部 CAS 防重入）。
		if err := r.faces.RunClustering(r.ctx); err != nil {
			zap.L().Warn("rebuild: face reclustering failed", zap.Error(err))
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
		if _, err := r.db.Exec(`INSERT INTO photos_meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			mlModelGenKey, common.MLModelGen); err != nil {
			zap.L().Warn("rebuild: write ml_model_gen failed", zap.Error(err))
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
				// 旧人脸行先删（face_detections 是 INSERT 而非 upsert，
				// 不删会翻倍；face_person 经 FK CASCADE 一并清理）。
				if _, err := r.db.Exec(`DELETE FROM face_detections WHERE asset_id=?`, t.id); err != nil {
					zap.L().Warn("rebuild: clear faces failed", zap.String("asset", t.id), zap.Error(err))
				}
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
