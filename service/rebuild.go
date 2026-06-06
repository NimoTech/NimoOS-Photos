package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	rows, err := r.db.Query(`SELECT id, file_path FROM assets WHERE status='indexed' AND deleted_at IS NULL`)
	if err != nil {
		zap.L().Warn("rebuild: query assets failed", zap.Error(err))
		return
	}
	var targets []rebuildTarget
	for rows.Next() {
		var t rebuildTarget
		if err := rows.Scan(&t.id, &t.path); err == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()

	started := time.Now()
	total := int64(len(targets))
	var processed int64
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
				r.indexer.ForceReprocess(t.path, processOpts{force: true, skipExif: true, skipThumb: true})
				atomic.AddInt64(&processed, 1)
				publish("running")
			}
		}()
	}
	for _, t := range targets {
		queue <- t
	}
	close(queue)
	wg.Wait()

	if r.ctx.Err() != nil {
		return // 服务关闭：不发 final，残留 running task 随服务消失
	}

	// 人脸重聚类（内部 CAS 防重入）。
	if err := r.faces.RunClustering(r.ctx); err != nil {
		zap.L().Warn("rebuild: face reclustering failed", zap.Error(err))
	}

	if _, err := r.db.Exec(`INSERT INTO photos_meta(key,value) VALUES('index_last_rebuilt',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		zap.L().Warn("rebuild: write meta failed", zap.Error(err))
	}

	publish("done")
	go func() {
		time.Sleep(taskCleanupDelay)
		r.reg.Remove(taskID)
	}()
}
