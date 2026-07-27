package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	sqlite3 "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// captionLister 是 Puller 依赖的最小接口,只取 ListCaptions 一个方法,方便
// 测试注入 fake(见 captionpull_test.go 的 fakeLister),避免直接依赖
// parserclient.Client 具体类型。
type captionLister interface {
	ListCaptions(ctx context.Context, offset string) ([]parserclient.CaptionItem, string, error)
}

// Puller 周期性从 NimoOS-Parser 拉取 caption 全量并 diff-upsert 进本地
// asset_caption 表(照片知识库子项目二的回流侧;caption 消费/检索是后续
// 子项目,这里只负责把数据落到本地)。
//
// 全部 best-effort:Parser 未部署 / 网络失败 / 503(qdrant 不可用)都只是
// 本轮 PullOnce 直接返回 err,调用方挂点仅记日志,不向上传播,不影响
// Photos 索引/搜索等主流程。
//
// 生命周期自洽:
//   - Parser 侧 caption 更新(重新生成)→ mtime_ms 变大,下一轮回流据此
//     覆盖本地旧文本;mtime 未变则跳过,避免无意义写盘。
//   - 资产被删除 → asset_caption 靠 asset_id 外键 ON DELETE CASCADE 自动
//     级联清理,本包无需关心。
//   - 资产被恢复(回收站恢复等)→ 只要资产行还在(级联未触发),旧 caption
//     行原样保留;Parser 侧后续若有更新,mtime 覆盖会自然生效。
//   - 孤儿(Parser 已生成 caption,但本地 assets 表暂无该 id,比如两侧删除
//     通知有时间差)→ 外键约束下 INSERT 失败,跳过继续处理下一条,不中断
//     整轮拉取。
type Puller struct {
	db     *sql.DB
	lister captionLister
}

// NewPuller 构造 Puller。lister 通常是 parserclient.New(cfg.RuntimePath)
// (与 CaptionFeeder 共用同一个 parserclient.Client 实例即可,ListCaptions
// 与 IngestAsset/DeleteAsset 走同一份 discoveryFile/http.Client)。
func NewPuller(db *sql.DB, lister captionLister) *Puller {
	return &Puller{db: db, lister: lister}
}

// localMtime 查询本地 asset_caption 表中某资产当前记录的 mtime_ms;
// 若尚无记录,ok=false。
func (p *Puller) localMtime(ctx context.Context, assetID string) (mtime int64, ok bool, err error) {
	err = p.db.QueryRowContext(ctx, `SELECT mtime_ms FROM asset_caption WHERE asset_id=?`, assetID).Scan(&mtime)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return mtime, true, nil
}

// PullOnce 拉取 Parser 侧全量 caption 并 diff-upsert 进本地表:分页游标
// 循环拉到底,每条与本地 mtime_ms 比对,本地缺失或 Parser 侧 mtime 更大
// 才写入(ON CONFLICT 覆盖);写入失败时精确区分:仅 SQLITE_CONSTRAINT_
// FOREIGNKEY(真孤儿资产)跳过继续,其它错误(SQLITE_BUSY 超时、磁盘 I/O
// 等真实故障)整轮直接返回 err,避免把故障误记为孤儿而静默吞掉。
//
// lister 出错(Parser 未部署 / 网络失败 / 非 2xx)同样整轮直接返回 err、
// 已写入的 upserted 计数原样返回——调用方(挂点)按 best-effort 语义仅
// 记日志,不向上传播致命错误。
func (p *Puller) PullOnce(ctx context.Context) (upserted int, err error) {
	offset := ""
	for {
		items, next, lerr := p.lister.ListCaptions(ctx, offset)
		if lerr != nil {
			return upserted, lerr
		}
		for _, it := range items {
			localMs, ok, qerr := p.localMtime(ctx, it.AssetID)
			if qerr != nil {
				return upserted, qerr
			}
			if ok && it.MtimeMs <= localMs {
				continue // 本地已是同版本或更新,跳过,避免无意义写盘
			}
			_, werr := p.db.ExecContext(ctx, `
				INSERT INTO asset_caption(asset_id, text, mtime_ms, fetched_at)
				VALUES(?, ?, ?, CURRENT_TIMESTAMP)
				ON CONFLICT(asset_id) DO UPDATE SET
					text       = excluded.text,
					mtime_ms   = excluded.mtime_ms,
					fetched_at = excluded.fetched_at`,
				it.AssetID, it.Text, it.MtimeMs)
			if werr != nil {
				var sqliteErr sqlite3.Error
				if errors.As(werr, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintForeignKey {
					// 真孤儿:本地 assets 无此 id(两侧删除通知有时间差属正常
					// 竞态)→ 跳过继续,不中断整轮拉取。
					zap.L().Debug("caption pull: 写入跳过(孤儿资产,外键约束失败)",
						zap.String("asset_id", it.AssetID), zap.Error(werr))
					continue
				}
				// 非外键错误(SQLITE_BUSY 超时、磁盘 I/O 等真实故障)不能当孤儿
				// 静默吞掉,否则会抹平故障信号——整轮直接返回 err,交给挂点的
				// 既有 Warn 日志路径处理。
				return upserted, werr
			}
			upserted++
		}
		if next == "" {
			break
		}
		offset = next
	}
	return upserted, nil
}
