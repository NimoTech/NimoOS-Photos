package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"go.uber.org/zap"
)

// captionSink 是 CaptionFeeder 依赖的鸭子类型接口，只取 parserclient.Client
// 用到的两个方法，便于测试注入 recordingSink 之类的假实现。
type captionSink interface {
	IngestAsset(ctx context.Context, assetID, imagePath, mime, takenAt, place string) error
	DeleteAsset(ctx context.Context, assetID string) error
}

// CaptionFeeder 把已索引资产投喂给 Parser 生成 caption（照片知识库子项目二）。
// 全部 best-effort：失败不影响索引/删除主流程；Parser 未部署时静默跳过。
//
// 刻意不上 TaskRegistry：投喂本身毫秒级，真正慢的是 Parser 侧消化
// （单张约 35s），若挂任务栏会造成"秒级完成但内容半小时后才可搜"的假完成
// 观感，对用户是负反馈而非有用信息。
type CaptionFeeder struct {
	db       *sql.DB
	sink     captionSink
	thumbDir string
	running  atomic.Bool
	rerun    atomic.Bool
}

// NewCaptionFeeder 构造 CaptionFeeder。sink 通常是 parserclient.New(cfg.RuntimePath)，
// thumbDir 与 Indexer 共用同一份缩略图目录（取 large.jpg 作为投喂图像）。
func NewCaptionFeeder(db *sql.DB, sink captionSink, thumbDir string) *CaptionFeeder {
	return &CaptionFeeder{db: db, sink: sink, thumbDir: thumbDir}
}

// feedInfo 查询投喂 Parser 所需的 mime/拍摄时间/地点文本。
// place 为 "City, Country" 形式，任一为空只拼存在的那一项。
func (f *CaptionFeeder) feedInfo(ctx context.Context, assetID string) (mime, takenAt, place string, err error) {
	var mimeNS sql.NullString
	var takenAtT sql.NullTime
	var city, country sql.NullString
	err = f.db.QueryRowContext(ctx, `
		SELECT a.mime_type, a.taken_at, g.city, g.country
		FROM assets a
		LEFT JOIN asset_geo g ON g.asset_id = a.id
		WHERE a.id = ?`, assetID).Scan(&mimeNS, &takenAtT, &city, &country)
	if err != nil {
		return "", "", "", err
	}
	mime = mimeNS.String
	if takenAtT.Valid {
		takenAt = takenAtT.Time.Format("2006-01-02")
	}
	place = joinPlace(city.String, country.String)
	return mime, takenAt, place, nil
}

// joinPlace 把 city/country 拼成 "City, Country"；任一为空只保留存在的一项。
func joinPlace(city, country string) string {
	switch {
	case city != "" && country != "":
		return city + ", " + country
	case city != "":
		return city
	case country != "":
		return country
	default:
		return ""
	}
}

// FeedOne 把单个资产投喂给 Parser：查载荷 → 投喂 → 成功置 caption_synced=1。
// 供索引内联钩子（SetOnIndexed）调用。任何失败都不影响调用方——本方法从不
// 返回 error，只在需要留痕时打日志；ErrParserUnavailable 完全静默（Parser
// 未部署是正常状态，不能刷日志)。
func (f *CaptionFeeder) FeedOne(ctx context.Context, assetID string) {
	mime, takenAt, place, err := f.feedInfo(ctx, assetID)
	if err != nil {
		// 资产在触发投喂后到查询前被删除/软删属良性竞态（比如用户几乎同时
		// 删了这张照片），不值得 Warn，Debug 留痕即可。
		zap.L().Debug("caption feed: 查询资产信息失败", zap.String("asset_id", assetID), zap.Error(err))
		return
	}
	imagePath := filepath.Join(f.thumbDir, assetID, "large.jpg")
	if err := f.sink.IngestAsset(ctx, assetID, imagePath, mime, takenAt, place); err != nil {
		if errors.Is(err, parserclient.ErrParserUnavailable) {
			return
		}
		zap.L().Warn("caption 投喂失败", zap.String("asset_id", assetID), zap.Error(err))
		return
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE assets SET caption_synced=1 WHERE id=?`, assetID); err != nil {
		zap.L().Warn("caption_synced 置位失败", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// OnRestore 把资产的 caption_synced 置回 0，供回收站恢复流程调用（Task 4 用）：
// 恢复后的资产需要重新投喂一次，Backfill 下一轮会自然捡起。
func (f *CaptionFeeder) OnRestore(assetID string) {
	if _, err := f.db.Exec(`UPDATE assets SET caption_synced=0 WHERE id=?`, assetID); err != nil {
		zap.L().Warn("caption_synced 复位失败", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// queryPending 列出待投喂资产 id：已索引、未软删、源文件可读（不在离线盘上）、
// 尚未同步过。
func (f *CaptionFeeder) queryPending(ctx context.Context) ([]string, error) {
	rows, err := f.db.QueryContext(ctx, `
		SELECT id FROM assets
		WHERE caption_synced = 0 AND status = 'indexed' AND deleted_at IS NULL AND offline = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Backfill 对所有欠投喂的已索引资产补跑投喂。CAS+rerunPending 骨架照
// Embedder.BackfillOCR：并发调用安全，第二次调用立即返回 nil，但置位
// rerun，由进行中的那轮结束后自动再跑一轮（重新查询目标）。
//
// 先探一次可用性：本轮首次真正调用 sink 若命中 ErrParserUnavailable，说明
// Parser 未部署，直接整轮静默返回（不留任何日志、不继续查后续资产）——这
// 是常态（多数机器不装 Parser），不能每次补扫都刷屏。短路锚点是"首次调用
// sink"而非列表下标 0：详见 feedBatch 内的说明。
func (f *CaptionFeeder) Backfill(ctx context.Context) error {
	if !f.running.CompareAndSwap(false, true) {
		f.rerun.Store(true)
		return nil
	}
	defer f.running.Store(false)

	for {
		if err := f.backfillOnce(ctx); err != nil {
			return err
		}
		if !f.rerun.CompareAndSwap(true, false) {
			return nil
		}
	}
}

// backfillOnce 是 Backfill 的单轮主体，不含并发防重与 rerun 循环。
func (f *CaptionFeeder) backfillOnce(ctx context.Context) error {
	ids, err := f.queryPending(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return f.feedBatch(ctx, ids)
}

// feedBatch 对给定 id 列表逐个投喂。从 backfillOnce 中拆出，便于测试直接
// 注入 ids（含不存在的 id，模拟 feedInfo 因资产竞态被删的场景）而不必依赖
// 真实并发时序。
//
// 短路判断锚定在"本轮首次真正调用 sink"，而不是列表下标 0：若开头几个 id
// 的 feedInfo 先失败（资产被并发删除/软删等良性竞态，见下方 continue 分
// 支），真正命中 ErrParserUnavailable 的可能是后续下标——这种情况下仍要
// 判定为"Parser 未部署"整轮静默短路，不能因为下标非 0 就漏判，否则会在
// 未部署 Parser 的机器上打出一条汇总日志，违背"零日志"诉求。
//
// 若非首次调用 sink 才命中 Unavailable，说明 Parser 在本轮开始时是可用
// 的、中途才掉线（比如补扫过程中重启了 Parser 容器）——这属于正常运维场
// 景，中断循环避免对已知不可用的 Parser 继续逐个重试，但保留汇总日志（已
// 有真实投喂发生，值得留痕，不算"零部署"静默场景）。
func (f *CaptionFeeder) feedBatch(ctx context.Context, ids []string) error {
	var fed, failed int64
	firstSinkCall := true
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		mime, takenAt, place, ierr := f.feedInfo(ctx, id)
		if ierr != nil {
			// 资产在被 queryPending 选中后到这里被删除/软删属良性竞态，不算
			// 作一次 sink 尝试，也不足以判断 Parser 是否部署，计入 failed
			// 后继续下一条。
			failed++
			continue
		}
		imagePath := filepath.Join(f.thumbDir, id, "large.jpg")
		serr := f.sink.IngestAsset(ctx, id, imagePath, mime, takenAt, place)
		isFirstSinkCall := firstSinkCall
		firstSinkCall = false
		if serr != nil {
			if errors.Is(serr, parserclient.ErrParserUnavailable) {
				if isFirstSinkCall {
					// 本轮首次真正调用 sink 就不可用：Parser 没部署，整轮
					// 静默短路，不留汇总日志。
					return nil
				}
				// 非首次命中 Unavailable：Parser 中途掉线，中断循环但保留
				// 汇总日志（正常运维场景，值得留痕）。
				break
			}
			failed++
			continue
		}
		if _, uerr := f.db.ExecContext(ctx, `UPDATE assets SET caption_synced=1 WHERE id=?`, id); uerr != nil {
			failed++
			continue
		}
		fed++
	}
	zap.L().Info("caption 补扫完成", zap.Int64("fed", fed), zap.Int64("failed", failed))
	return nil
}
