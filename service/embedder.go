package service

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"
)

const defaultEmbedderPollInterval = 30 * time.Second

type Embedder struct {
	db           *sql.DB
	ml           MLProvider
	indexer      *Indexer
	reg          *TaskRegistry
	lastReady    atomic.Bool
	running      atomic.Bool
	pollInterval time.Duration
}

func NewEmbedder(db *sql.DB, ml MLProvider, idx *Indexer, reg *TaskRegistry) *Embedder {
	return &Embedder{
		db: db, ml: ml, indexer: idx, reg: reg,
		pollInterval: defaultEmbedderPollInterval,
	}
}

func (e *Embedder) SetPollInterval(d time.Duration) { e.pollInterval = d }

// queryMissing 列出 status='indexed' 但 asset_clip_idx 缺行的 asset 路径。
func (e *Embedder) queryMissing(ctx context.Context) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.file_path FROM assets a
        LEFT JOIN asset_clip_idx i ON i.asset_id = a.id
        WHERE a.status = 'indexed' AND i.asset_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// hasEmbeddingForPath 反查指定 path 是否已在 asset_clip_idx 有行。
// 用 path 反查而非 asset_id，是因为 Backfill 主循环只持有 path 列表。
func (e *Embedder) hasEmbeddingForPath(path string) bool {
	var n int
	_ = e.db.QueryRow(`
        SELECT 1 FROM asset_clip_idx i
        JOIN assets a ON a.id = i.asset_id
        WHERE a.file_path = ? LIMIT 1`, path).Scan(&n)
	return n == 1
}

// Backfill 对所有 status='indexed' 但缺 CLIP 向量的 asset 补跑 ML。
// 并发调用安全：第二次调用会立即返回 nil。
func (e *Embedder) Backfill(ctx context.Context) error {
	if !e.running.CompareAndSwap(false, true) {
		return nil
	}
	defer e.running.Store(false)

	paths, err := e.queryMissing(ctx)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}

	taskID := fmt.Sprintf("embedding_%d", time.Now().UnixNano())
	started := time.Now()
	total := int64(len(paths))
	pubRunning := func(processed int64) {
		e.reg.Upsert(Task{
			ID:        taskID,
			Type:      "embedding",
			Label:     "生成 AI 索引",
			Total:     total,
			Current:   processed,
			Progress:  float64(processed) / float64(total),
			Status:    "running",
			StartedAt: started,
		})
	}
	pubRunning(0)

	var processed, success, failed int64
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		e.indexer.ForceReprocess(p, processOpts{force: true, skipExif: true, skipThumb: true})
		processed++
		if e.hasEmbeddingForPath(p) {
			success++
		} else {
			failed++
		}
		pubRunning(processed)
	}

	// 若 ctx 已取消（用户主动取消或服务关闭），不发 final task——
	// 部分完成不等于完成，registry 里残留的 running task 会随服务关闭自然消失。
	if ctx.Err() != nil {
		return ctx.Err()
	}

	final := Task{
		ID:        taskID,
		Type:      "embedding",
		Label:     "生成 AI 索引",
		Total:     total,
		Current:   success,
		StartedAt: started,
	}
	if success == 0 && failed > 0 {
		final.Status = "error"
		final.Error = "ML 服务在补跑过程中失联，请检查服务状态"
	} else {
		final.Status = "done"
		final.Progress = 1
		if failed > 0 {
			final.Label = fmt.Sprintf("生成 AI 索引（失败 %d 张）", failed)
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

// Run 主循环：每隔 pollInterval 检查 ML ready 状态，
// 检测到 false→true 跳变时触发一次 Backfill（goroutine 异步执行）。
// 服务启动时如果 ML 已经就绪，第一次 tick 的 prev=false 也会触发——符合 spec §5.2。
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

// tick 检测 ML ready 跳变，有跳变时异步触发 Backfill。
func (e *Embedder) tick(ctx context.Context) {
	ready := e.ml.IsReady()
	prev := e.lastReady.Swap(ready)
	if ready && !prev {
		go func() { _ = e.Backfill(ctx) }()
	}
}
