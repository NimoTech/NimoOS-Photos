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

// aestheticHeadVerKey 是 photos_meta 中登记美学头版本的键。
const aestheticHeadVerKey = "aesthetic_head_ver"

// EnsureAestheticHeadVer 对齐库内头版本:不符时同一事务内全库分数置 NULL 并盖章。
// 与 ml_model_gen 的「成功后盖章」不同:置 NULL 已原子清除全部旧分,无脏数据窗口,
// 提前盖章安全;重打靠 BackfillAesthetic 的 NULL 查询自恢复。
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

	// downStreak 是连续探测到 ML 未就绪的次数,用于把单次 /ping 抖动与真掉线
	// 区分开(见 mlDownStreakThreshold 与 observeReady)。
	downStreak atomic.Int32

	// gate 是恢复链的节流闸,与批次完成钩子共用同一个实例(service.go 注入),
	// 以保证「无论由哪个触发源发起,重补跑链每个窗口最多跑一轮」。
	// nil 表示不节流(单测与未接线路径)。
	gate *backfillGate

	// aestheticRunning / aestheticRerunPending / aestheticHead 支撑 BackfillAesthetic
	// 的并发防重与 rerun 语义,与 ocrRunning/ocrRerunPending 同款(见下方字段注释)。
	aestheticRunning      atomic.Bool
	aestheticRerunPending atomic.Bool
	aestheticHead         *aesthetic.Head

	// rerunPending / ocrRerunPending 记录「补跑运行中又收到了一次触发」。
	// 不能像以前那样让撞上 CAS 的第二次调用静默返回 nil:进行中的那轮可能早已
	// 查过目标列表,查不到调用方刚变成可补的资产(典型:MountGuard 刚把插回的
	// 盘上的资产标回 offline=0),吞掉触发等于治愈永远不发生。置位后,当前轮
	// 结束时重新查询、再跑一轮。
	rerunPending    atomic.Bool
	ocrRerunPending atomic.Bool

	// onRecovered 在 ML ready 恢复链尾（Backfill → reembed → BackfillOCR 之后）
	// 被调用一次，覆盖 ML 掉线期间积压的人脸检测欠账。用函数字段注入而非直接
	// import FaceService（同 MountGuard 的 SetBackfill/SetBackfillOCR 模式），
	// 避免 Embedder 与 FaceService 产生类型耦合；service.go 接线为
	// faces.RunPipeline。为 nil 时（未接线 / 测试）安全跳过。
	onRecovered func(context.Context)
}

func NewEmbedder(db *sql.DB, ml MLProvider, idx *Indexer, reg *TaskRegistry) *Embedder {
	return &Embedder{
		db: db, ml: ml, indexer: idx, reg: reg,
		pollInterval: defaultEmbedderPollInterval,
	}
}

func (e *Embedder) SetPollInterval(d time.Duration) { e.pollInterval = d }

// SetGate 注入恢复链的节流闸。传入与批次完成钩子同一个实例,才能保证两个
// 触发源合起来每个窗口只跑一轮重补跑链。
func (e *Embedder) SetGate(g *backfillGate) { e.gate = g }

// runGated 按节流闸执行 fn;未注入闸时直接执行。
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

// clipTarget 是一条 CLIP 补跑候选。带上 id 是为了走「只读缩略图」的轻路径
// (embedClip 按 asset id 找 small.jpg)与失败台账记账,不必再按 path 反查。
type clipTarget struct {
	id   string
	path string
}

// queryMissing 列出 status='indexed' 但 asset_clip_idx 缺行的 asset。
// now 用于失败台账的冷却判定(见 backfillretry.go):处于冷却期的资产本轮
// 不入选,避免永久失败的资产每轮都被重新选中。
func (e *Embedder) queryMissing(ctx context.Context, now time.Time) ([]clipTarget, error) {
	// a.offline=0: an asset on a currently-unplugged removable drive can't be
	// read, so retrying it here would just burn CPU on a guaranteed failure
	// every poll interval. MountGuard re-triggers Backfill right after the
	// drive is reinserted, so the gap is closed the moment the file is
	// reachable again.
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.id, a.file_path FROM assets a
        LEFT JOIN asset_clip_idx i ON i.asset_id = a.id
        WHERE a.status = 'indexed' AND a.offline = 0 AND i.asset_id IS NULL`+
		backfillCooldownSQL("a"),
		string(backfillCLIP), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []clipTarget
	for rows.Next() {
		var t clipTarget
		if err := rows.Scan(&t.id, &t.path); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
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
	for _, tg := range targets {
		if ctx.Err() != nil {
			break
		}
		err := e.embedOne(tg)
		processed++
		if err == nil && e.hasEmbeddingForPath(tg.path) {
			success++
			clearBackfillFailure(e.db, backfillCLIP, tg.id)
		} else {
			failed++
			if err == nil {
				err = fmt.Errorf("补跑后仍无向量")
			}
			recordBackfillFailure(e.db, backfillCLIP, tg.id, time.Now(), err)
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
		final.SetError(TaskErrMLLostDuringBackfill, nil)
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

// embedOne 补一个资产的 CLIP 向量。
//
// 轻路径优先:CLIP 向量的唯一输入是 small.jpg 缩略图(见 embedClip 的注释——
// 为了让排序与用户看到的那一帧一致,索引期与补跑期都用缩略图),缩略图在就
// 只读它,一个源文件字节都不碰。旧实现无条件走 ForceReprocess 重管线,而
// force=true 会绕过 processFileInternal 的 stat 快速跳过与 checksum 短路,
// 对每个候选整读一遍源文件算 SHA-256,视频还额外抽关键帧 + ffprobe——补一个
// 几百 KB 的向量要读几个 GB 的源视频,纯浪费。生产上这就是「7.3T 素材盘被
// 每轮补跑整盘重读、磁盘 24h 满速顺序读」的直接来源。
//
// 缩略图缺失时仍必须回落重管线:那是唯一能重新生成缩略图的路径,否则从未
// 出过缩略图的资产会永远补不出向量。
func (e *Embedder) embedOne(tg clipTarget) error {
	if e.indexer.hasSmallThumb(tg.id) {
		return e.indexer.embedClip(tg.id, nil)
	}
	e.indexer.ForceReprocess(tg.path, processOpts{force: true, skipExif: true, skipThumb: true})
	return nil
}

// queryMissingOCR 列出 status='indexed' 但 OCR 数据不完整的 asset：
// 要么 asset_ocr 缺行（从未跑过 OCR——即使没识别到文字也会写 text 为空字符串的行），
// 要么 coverage 为 NULL（旧版跑的 OCR 没存文字框面积，需要重跑补齐），
// 要么 boxes_ver=0（旧版没把逐行坐标存进 asset_ocr_lines，需重跑供搜索命中高亮）。
// now 用于失败台账的冷却判定,语义同 queryMissing。
func (e *Embedder) queryMissingOCR(ctx context.Context, now time.Time) ([]ocrTarget, error) {
	// a.offline=0: same reasoning as queryMissing above — skip assets whose
	// source is unreachable because their removable drive is unplugged.
	rows, err := e.db.QueryContext(ctx, `
        SELECT a.id, a.file_path, COALESCE(a.mime_type,'') LIKE 'video/%'
        FROM assets a
        LEFT JOIN asset_ocr o ON o.asset_id = a.id
        WHERE a.status = 'indexed' AND a.deleted_at IS NULL AND a.offline = 0
          AND COALESCE(a.mime_type,'') NOT LIKE 'video/%'
          AND (o.asset_id IS NULL OR o.coverage IS NULL OR COALESCE(o.boxes_ver,0) = 0)`+
		backfillCooldownSQL("a"),
		string(backfillOCR), now.UnixMilli())
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

// ocrOutcome 是 OCR 补跑对单个目标的一次尝试结果。err==nil 表示成功。
// 逐条收集而非只记计数,是为了在「最终一遍」结束后按条写失败退避台账。
type ocrOutcome struct {
	target ocrTarget
	err    error
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
			Label:     "识别图片文字",
			Total:     total,
			Current:   processed,
			Progress:  float64(processed) / float64(total),
			Status:    "running",
			StartedAt: started,
		})
	}
	pubRunning(0)

	// pass 跑一遍给定目标,返回逐条结果与处理/失败计数(readFail=源文件读不到,
	// ocrFail=ML 调用失败)。outcomes 交给调用方在「最终一遍」之后统一记账:
	// 首遍疑似 ML 未热好时还会重试一遍,不能拿首遍的失败去写退避台账。
	pass := func(ts []ocrTarget) (outcomes []ocrOutcome, processed, failed, readFail, ocrFail int64) {
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
				if rerr == nil {
					rerr = fmt.Errorf("源图为空: %s", src)
				}
				outcomes = append(outcomes, ocrOutcome{target: t, err: rerr})
				readFail++
				failed++
				processed++
				pubRunning(processed)
				continue
			}
			// 超大原图守卫(与 detectFaceScanTarget / 索引内联 OCR 同一套):
			// 超过 PIL 上限的图片发原图必然 500 且每次恢复链都重试,降级用缩略图。
			if !t.isVideo && oversizedForML(data) {
				if thumb := readLargeOrSmallThumb(e.indexer.thumbDir, t.id); len(thumb) > 0 {
					data = thumb
				} else {
					zap.L().Warn("OCR 补跑:超大图且无缩略图可降级,跳过",
						zap.String("asset_id", t.id), zap.String("path", t.path))
					outcomes = append(outcomes, ocrOutcome{
						target: t, err: fmt.Errorf("超过 ML 像素上限且无缩略图可降级"),
					})
					readFail++
					failed++
					processed++
					pubRunning(processed)
					continue
				}
			}
			oerr := e.indexer.ocrAsset(t.id, data)
			outcomes = append(outcomes, ocrOutcome{target: t, err: oerr})
			if oerr != nil {
				ocrFail++
				failed++
			} else if derr := e.indexer.computeDocVerdict(t.id); derr != nil {
				zap.L().Warn("doc verdict after ocr backfill failed", zap.String("asset_id", t.id), zap.Error(derr))
			}
			processed++
			pubRunning(processed)
		}
		return
	}

	outcomes, processed, failed, readFail, ocrFail := pass(targets)

	// 首遍因 ML 全失败(疑似重启瞬间 OCR 模型还没热好):等一会重试一次,仍失败
	// 才报错。避免每次重部署都弹一个其实是「ML 未热好」的假失败。
	//
	// 重试的是同一批 targets,不再重新查询:候选查询现在带失败台账的冷却过滤
	// (queryMissingOCR),而本轮的失败要等最终一遍结束后才记账,此刻重新查询
	// 拿回来的仍是同一批,白跑一次 SQL;而且这个分支只在「全部失败」时才进,
	// 没有已成功的目标需要被剔除。
	if processed > 0 && failed == processed && ocrFail > 0 && ctx.Err() == nil {
		zap.L().Info("OCR 补跑首遍全失败(疑似 ML 未就绪),稍后重试一次",
			zap.Int64("ml_fail", ocrFail))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ocrBackfillRetryDelay):
		}
		pubRunning(0)
		outcomes, processed, failed, readFail, ocrFail = pass(targets)
	}

	if ctx.Err() != nil {
		// 被取消:不记账。停服/取消导致的失败不该让资产背上退避冷却。
		return ctx.Err()
	}

	// 以最终一遍的逐条结果记账:成功清账,失败按退避阶梯推迟下次入选。
	// 这是「同一批 62 张每轮全失败、每轮把原图重读一遍」的止血点。
	now := time.Now()
	for _, o := range outcomes {
		if o.err == nil {
			clearBackfillFailure(e.db, backfillOCR, o.target.id)
		} else {
			recordBackfillFailure(e.db, backfillOCR, o.target.id, now, o.err)
		}
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

// BackfillDocVerdicts 为「OCR 行坐标已存(boxes_ver=1)、doc 判定未算(doc_ver=0)、
// 图向量已就绪」的资产补算 doc 分类混合判定。纯本地数学(提示词向量最多一次文本
// 嵌入),毫秒级/张,因此不挂 TaskRegistry、不发任务事件(与跑推理的 BackfillOCR
// 不同)。无向量的资产不入选,等 CLIP Backfill 补齐向量后的下一轮钩子再收敛。
func (e *Embedder) BackfillDocVerdicts(ctx context.Context) error {
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

// SetAestheticHead 注入美学评分头;nil 时 BackfillAesthetic 是 no-op。
func (e *Embedder) SetAestheticHead(h *aesthetic.Head) { e.aestheticHead = h }

// BackfillAesthetic 为「有 CLIP 向量但 aesthetic_score IS NULL」的资产补算美学分。
// 纯本地矩阵乘,不依赖 ML 在线;不过滤 offline(打分只读库内向量,不碰文件)。
// 并发防重与 rerun 语义同 BackfillOCR。
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

// backfillAestheticOnce 是 BackfillAesthetic 的单轮主体,不含并发防重与 rerun 循环。
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
			ID: taskID, Type: "aesthetic", Label: "评估照片美学分",
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
			// 查询和读取之间向量被删(如 rebuild 竞态):跳过,留 NULL 下轮收敛。
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
			zap.L().Warn("aesthetic backfill: 写分失败", zap.String("asset_id", id), zap.Error(err))
			failed++
		}
		processed++
		pubRunning(processed)
	}

	final := Task{
		ID: taskID, Type: "aesthetic", Label: "评估照片美学分",
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

// mlDownStreakThreshold 是认定「ML 真的掉线」所需的连续探测失败次数。
//
// IsReady 是 3 秒超时的 /ping。而 ML 冷加载 / 满负载时 /ping 延迟实测能到
// ~2.5s(见 mlwatchdog.go 的注释),偶发一次超时并不代表后端掉线。若把单次
// 抖动当掉线,负载一回落就是一次 false→true 跳变、触发整条恢复链,恢复链再把
// ML 打满——生产上「immich_ml 返回 500/EOF 而补跑每轮重来」的正反馈就是这么
// 转起来的。要求连续 2 次失败才认定掉线,单次抖动被吃掉;真掉线最多晚一个
// 轮询周期被发现,而后续动作(补跑)本来就不是实时的。
const mlDownStreakThreshold = 2

// observeReady 消费一次 ML 就绪探测结果,返回是否发生了「掉线→恢复」跳变
// (即是否应当触发一轮恢复链)。带连续失败去抖,见 mlDownStreakThreshold。
// 初始状态视为「未就绪」,所以启动后第一次探测到就绪会触发一轮——这是刻意的,
// 用来补齐上次运行期间积压的欠账。
func (e *Embedder) observeReady(ready bool) bool {
	if !ready {
		if e.downStreak.Add(1) >= mlDownStreakThreshold {
			e.lastReady.Store(false)
		}
		return false
	}
	e.downStreak.Store(0)
	return !e.lastReady.Swap(true)
}

// tick 检测 ML ready 跳变，有跳变时异步触发 Backfill。
func (e *Embedder) tick(ctx context.Context) {
	if e.observeReady(e.ml.IsReady()) {
		go e.runGated(func() {
			// Backfill first (fills assets that never got an embedding), then the
			// one-time re-embed of all existing assets from their thumbnails,
			// then OCR for assets indexed before OCR support existed, then doc
			// verdicts for OCR'd assets missing the mixed-criteria judgment
			// (BackfillDocVerdicts), then aesthetic scores for assets whose CLIP
			// vector arrived while ML was down (BackfillAesthetic，纯本地计算，
			// 不依赖 ML，但仍挂在同一条恢复链上顺带收敛)，最后 faces
			// (RunPipeline，via onRecovered) — covers detection backlog
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
