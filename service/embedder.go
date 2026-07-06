package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const defaultEmbedderPollInterval = 30 * time.Second

// ocrBackfillRetryDelay 是 OCR 补跑「首遍因 ML 全失败」后重试前的等待,给重启瞬间
// 尚未热好的 ML OCR 模型一点时间,避免把暂时性失败当成真失败弹给用户。
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

	// rerunPending / ocrRerunPending 记录「补跑运行中又收到了一次触发」。
	// 不能像以前那样让撞上 CAS 的第二次调用静默返回 nil:进行中的那轮可能早已
	// 查过目标列表,查不到调用方刚变成可补的资产(典型:MountGuard 刚把插回的
	// 盘上的资产标回 offline=0),吞掉触发等于治愈永远不发生。置位后,当前轮
	// 结束时重新查询、再跑一轮。
	rerunPending    atomic.Bool
	ocrRerunPending atomic.Bool
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
	// a.offline=0: an asset on a currently-unplugged removable drive can't be
	// read, so retrying it here would just burn CPU on a guaranteed failure
	// every poll interval. MountGuard re-triggers Backfill right after the
	// drive is reinserted, so the gap is closed the moment the file is
	// reachable again.
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.file_path FROM assets a
        LEFT JOIN asset_clip_idx i ON i.asset_id = a.id
        WHERE a.status = 'indexed' AND a.offline = 0 AND i.asset_id IS NULL`)
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
// 并发调用安全：第二次调用立即返回 nil,但会置 rerunPending,由进行中的那轮
// 结束后自动再跑一轮(重新查询目标),保证触发不被吞掉。
//
// 已知的微小窗口:若置位恰好发生在当前轮最后一次 pending 检查之后、running
// 释放之前,这次触发要等到下一次 Backfill 调用(ML ready 跳变 / 挂载恢复 /
// 手动触发)才被消费。窗口极窄且这些触发源都会周期性出现,不做双重检查。
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

// backfillOnce 是 Backfill 的单轮主体(查询目标 + 逐个补跑 + 任务上报),
// 不含并发防重与 rerun 循环。
func (e *Embedder) backfillOnce(ctx context.Context) error {
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

// queryMissingOCR 列出 status='indexed' 但 OCR 数据不完整的 asset：
// 要么 asset_ocr 缺行（从未跑过 OCR——即使没识别到文字也会写 text='' 行），
// 要么 coverage 为 NULL（旧版跑的 OCR 没存文字框面积，需要重跑补齐）。
func (e *Embedder) queryMissingOCR(ctx context.Context) ([]ocrTarget, error) {
	// a.offline=0: same reasoning as queryMissing above — skip assets whose
	// source is unreachable because their removable drive is unplugged.
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.id, a.file_path, COALESCE(a.mime_type,'') LIKE 'video/%'
        FROM assets a
        LEFT JOIN asset_ocr o ON o.asset_id = a.id
        WHERE a.status = 'indexed' AND a.deleted_at IS NULL AND a.offline = 0
          AND COALESCE(a.mime_type,'') NOT LIKE 'video/%'
          AND (o.asset_id IS NULL OR o.coverage IS NULL)`)
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

// BackfillOCR 对所有缺 OCR 文本的 asset 补跑 ML OCR。
// 图片读原图（小票/文档的小字在缩略图分辨率下会丢失）；
// 视频没有现成关键帧文件，退而用 large.jpg（1280px）缩略图。
// 并发调用安全：第二次调用立即返回 nil,但会置 ocrRerunPending,由进行中的
// 那轮结束后自动再跑一轮——理由与窗口说明同 Backfill。
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

// backfillOCROnce 是 BackfillOCR 的单轮主体,不含并发防重与 rerun 循环。
func (e *Embedder) backfillOCROnce(ctx context.Context) error {
	targets, err := e.queryMissingOCR(ctx)
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
			Label:     "识别图片文字",
			Total:     total,
			Current:   processed,
			Progress:  float64(processed) / float64(total),
			Status:    "running",
			StartedAt: started,
		})
	}
	pubRunning(0)

	// pass 跑一遍给定目标,返回处理/失败计数(readFail=源文件读不到,ocrFail=ML 调用失败)。
	pass := func(ts []ocrTarget) (processed, failed, readFail, ocrFail int64) {
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
				// 源图/缩略图读不到(文件被删、缩略图缺失等),与 ML 无关。
				readFail++
				failed++
				processed++
				pubRunning(processed)
				continue
			}
			if oerr := e.indexer.ocrAsset(t.id, data); oerr != nil {
				ocrFail++
				failed++
			}
			processed++
			pubRunning(processed)
		}
		return
	}

	processed, failed, readFail, ocrFail := pass(targets)

	// 首遍因 ML 全失败(疑似重启瞬间 OCR 模型还没热好):等一会、对仍缺 OCR 的目标重试一次,
	// 仍失败才报错。避免每次重部署都弹一个其实是「ML 未热好」的假失败。
	if processed > 0 && failed == processed && ocrFail > 0 && ctx.Err() == nil {
		zap.L().Info("OCR 补跑首遍全失败(疑似 ML 未就绪),稍后重试一次",
			zap.Int64("ml_fail", ocrFail))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ocrBackfillRetryDelay):
		}
		if ts2, qerr := e.queryMissingOCR(ctx); qerr == nil && len(ts2) > 0 {
			total = int64(len(ts2))
			pubRunning(0)
			processed, failed, readFail, ocrFail = pass(ts2)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	final := Task{
		ID:        taskID,
		Type:      "ocr",
		Label:     "识别图片文字",
		Total:     total,
		Current:   processed - failed,
		StartedAt: started,
	}
	if processed > 0 && failed == processed {
		// 全部失败才报 error，并按真实原因给出准确信息(此前一律写「ML 失联」是误报)。
		final.Status = "error"
		switch {
		case ocrFail == 0 && readFail > 0:
			final.Error = fmt.Sprintf("%d 张照片的源文件无法读取，已跳过文字识别", readFail)
		case ocrFail > 0 && readFail == 0:
			final.Error = "ML 服务在补跑过程中失联，请检查服务状态"
		default:
			final.Error = fmt.Sprintf("文字识别补跑失败(源文件读取失败 %d 张、ML 失败 %d 张)", readFail, ocrFail)
		}
	} else {
		final.Status = "done"
		final.Progress = 1
		if failed > 0 {
			final.Label = fmt.Sprintf("识别图片文字（失败 %d 张）", failed)
		}
	}
	if failed > 0 {
		zap.L().Warn("OCR 补跑存在失败",
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
		go func() {
			// Backfill first (fills assets that never got an embedding), then the
			// one-time re-embed of all existing assets from their thumbnails,
			// then OCR for assets indexed before OCR support existed.
			_ = e.Backfill(ctx)
			e.reembedThumbnailsOnce()
			_ = e.BackfillOCR(ctx)
		}()
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
