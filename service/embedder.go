package service

import (
	"context"
	"database/sql"
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

// Backfill 占位：实际实现在 Task 8。
func (e *Embedder) Backfill(ctx context.Context) error { return nil }

// Run 占位：实际实现在 Task 9。
func (e *Embedder) Run(ctx context.Context) {}
