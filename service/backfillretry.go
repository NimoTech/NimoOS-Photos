package service

import (
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// backfillKind 标识一类补跑,是 backfill_failures 表的第一主键列。
// 三类补跑各自独立记账:同一资产的 CLIP 失败不该把它的 OCR 也拉进冷却。
type backfillKind string

const (
	backfillCLIP   backfillKind = "clip"
	backfillOCR    backfillKind = "ocr"
	backfillSprite backfillKind = "sprite"
)

// backfillRetryDelays 是「连续第 n 次失败」后的冷却阶梯。
//
// 为什么需要它:CLIP/OCR/sprite 三条补跑的候选查询都是「缺产物就选中」的纯
// SQL 判定,失败不留痕——一条永久失败的资产(损坏视频、超限图片、ML 长期
// 打不通)会在每一轮补跑里被重新选中、重新整读源文件、重新失败,candidate
// 集合永远不收敛。叠加批次末尾钩子(队列空闲 6 秒即算一批)后就是生产上实测
// 到的「磁盘 24 小时满速顺序读、进度零推进」。有了台账,失败资产按阶梯退出
// 候选集,盘能停下来;成功即清账,不会永久拉黑。
//
// 阶梯到 24h 封顶而不是彻底放弃:损坏源文件可能被用户替换、ML 模型可能被
//升级,永久拉黑会让这些自愈场景失效。
var backfillRetryDelays = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// backfillRetryDelay 返回第 failCount 次失败后应等待多久才允许再试。
// failCount 从 1 起算;非法值(≤0)按首次处理,超出阶梯长度钳在最后一档。
func backfillRetryDelay(failCount int) time.Duration {
	if failCount < 1 {
		failCount = 1
	}
	if failCount > len(backfillRetryDelays) {
		failCount = len(backfillRetryDelays)
	}
	return backfillRetryDelays[failCount-1]
}

// recordBackfillFailure 记一次补跑失败:计数 +1,并按新计数对应的退避档
// 把 next_retry_at 推到 now 之后。best-effort——记账失败只告警,不影响补跑
// 主流程(最坏退化成本次改动前的行为:下轮再试一次)。
func recordBackfillFailure(db *sql.DB, kind backfillKind, assetID string, now time.Time, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	nowMs := now.UnixMilli()
	// fail_count 在 UPDATE 分支里自增,next_retry_at 必须用自增后的值算退避,
	// 所以延迟表在 SQL 里展开成 CASE(纯数据,与 backfillRetryDelays 一一对应,
	// 改阶梯时两处必须同步——单测 TestRecordBackfillFailure_AccumulatesAndSchedules
	// 校验的是这条 SQL 的实际结果,阶梯不同步会红)。
	if _, err := db.Exec(`
		INSERT INTO backfill_failures(kind, asset_id, fail_count, last_fail_at, next_retry_at, last_error)
		VALUES(?,?,1,?,?,?)
		ON CONFLICT(kind, asset_id) DO UPDATE SET
		  fail_count    = fail_count + 1,
		  last_fail_at  = excluded.last_fail_at,
		  next_retry_at = excluded.last_fail_at + (
		      CASE MIN(fail_count + 1, ?)
		        WHEN 1 THEN ? WHEN 2 THEN ? WHEN 3 THEN ? WHEN 4 THEN ? ELSE ? END),
		  last_error    = excluded.last_error`,
		string(kind), assetID, nowMs, nowMs+backfillRetryDelay(1).Milliseconds(), msg,
		len(backfillRetryDelays),
		backfillRetryDelays[0].Milliseconds(), backfillRetryDelays[1].Milliseconds(),
		backfillRetryDelays[2].Milliseconds(), backfillRetryDelays[3].Milliseconds(),
		backfillRetryDelays[4].Milliseconds(),
	); err != nil {
		zap.L().Warn("补跑失败台账写入失败",
			zap.String("kind", string(kind)), zap.String("asset_id", assetID), zap.Error(err))
	}
}

// clearBackfillFailure 补跑成功后清账,让资产立刻恢复可补状态。
// best-effort:清账失败只告警(下次成功会再清一遍)。
func clearBackfillFailure(db *sql.DB, kind backfillKind, assetID string) {
	if _, err := db.Exec(`DELETE FROM backfill_failures WHERE kind=? AND asset_id=?`,
		string(kind), assetID); err != nil {
		zap.L().Warn("补跑失败台账清理失败",
			zap.String("kind", string(kind)), zap.String("asset_id", assetID), zap.Error(err))
	}
}

// backfillCooldownSQL 是拼进候选查询 WHERE 的冷却过滤片段。alias 是该查询里
// assets 表的别名(候选必须能取到 asset id)。绑定参数顺序:kind, nowMs。
// 三条补跑的候选查询共用这一份,过滤语义由构造保证一致。
func backfillCooldownSQL(alias string) string {
	return ` AND NOT EXISTS (
	        SELECT 1 FROM backfill_failures f
	        WHERE f.kind = ? AND f.asset_id = ` + alias + `.id AND f.next_retry_at > ?)`
}
