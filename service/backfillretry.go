package service

import (
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// backfillKind names one backfill chain in the failure ledger. Kinds are
// independent: a CLIP failure must not delay the same asset's OCR retry.
type backfillKind string

const (
	backfillCLIP   backfillKind = "clip"
	backfillOCR    backfillKind = "ocr"
	backfillSprite backfillKind = "sprite"
)

// backfillRetryDelays is the escalation ladder. Capped at 24h instead of a
// permanent blacklist: a replaced corrupt file or an upgraded ML backend
// must be able to self-heal without manual DB surgery.
var backfillRetryDelays = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

func backfillRetryDelay(failCount int) time.Duration {
	if failCount < 1 {
		failCount = 1
	}
	if failCount > len(backfillRetryDelays) {
		failCount = len(backfillRetryDelays)
	}
	return backfillRetryDelays[failCount-1]
}

// recordBackfillFailure bumps the ledger for one failed asset. Best-effort:
// ledger writes never fail the backfill itself. The CASE ladder below must
// stay in sync with backfillRetryDelays (guarded by
// TestRecordBackfillFailure_AccumulatesAndSchedules).
func recordBackfillFailure(db *sql.DB, kind backfillKind, assetID string, now time.Time, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	nowMs := now.UnixMilli()
	_, err := db.Exec(`
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
		backfillRetryDelays[4].Milliseconds())
	if err != nil {
		zap.L().Warn("backfill failure ledger write failed",
			zap.String("kind", string(kind)), zap.String("asset_id", assetID), zap.Error(err))
	}
}

// clearBackfillFailure removes the ledger row after a success so a recovered
// asset immediately returns to normal scheduling.
func clearBackfillFailure(db *sql.DB, kind backfillKind, assetID string) {
	if _, err := db.Exec(`DELETE FROM backfill_failures WHERE kind=? AND asset_id=?`,
		string(kind), assetID); err != nil {
		zap.L().Warn("backfill failure ledger clear failed",
			zap.String("kind", string(kind)), zap.String("asset_id", assetID), zap.Error(err))
	}
}

// backfillCooldownSQL returns a candidate-query fragment excluding assets in
// cooldown. Appends two bind parameters: string(kind), now.UnixMilli().
func backfillCooldownSQL(alias string) string {
	return ` AND NOT EXISTS (
        SELECT 1 FROM backfill_failures f
        WHERE f.kind = ? AND f.asset_id = ` + alias + `.id AND f.next_retry_at > ?)`
}
