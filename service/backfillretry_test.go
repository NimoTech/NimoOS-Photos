package service

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBackfillRetryDelay_Escalates 验证退避阶梯:首次失败冷却最短,
// 连续失败逐级拉长,超出阶梯长度后钳在最后一档(不无限增长)。
func TestBackfillRetryDelay_Escalates(t *testing.T) {
	require.Equal(t, 5*time.Minute, backfillRetryDelay(1))
	require.Equal(t, 15*time.Minute, backfillRetryDelay(2))
	require.Equal(t, time.Hour, backfillRetryDelay(3))
	require.Equal(t, 6*time.Hour, backfillRetryDelay(4))
	require.Equal(t, 24*time.Hour, backfillRetryDelay(5))
	require.Equal(t, 24*time.Hour, backfillRetryDelay(99), "超出阶梯必须钳在最后一档")
	require.Equal(t, 5*time.Minute, backfillRetryDelay(0), "非法计数按首次处理")
}

// TestRecordBackfillFailure_AccumulatesAndSchedules 验证同一 (kind, asset) 的
// 连续失败累加计数,并按当次计数对应的退避档写 next_retry_at。
func TestRecordBackfillFailure_AccumulatesAndSchedules(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	t0 := time.Unix(1_700_000_000, 0)

	recordBackfillFailure(db, backfillCLIP, id, t0, errors.New("ml down"))
	cnt, next := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 1, cnt)
	require.Equal(t, t0.Add(5*time.Minute).UnixMilli(), next)

	t1 := t0.Add(10 * time.Minute)
	recordBackfillFailure(db, backfillCLIP, id, t1, errors.New("ml down again"))
	cnt, next = readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 2, cnt)
	require.Equal(t, t1.Add(15*time.Minute).UnixMilli(), next)
}

// TestRecordBackfillFailure_KindsAreIndependent 验证同一资产在不同补跑
// 类型下的失败台账互不干扰(CLIP 失败不该把 OCR 也拉进冷却)。
func TestRecordBackfillFailure_KindsAreIndependent(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	t0 := time.Unix(1_700_000_000, 0)

	recordBackfillFailure(db, backfillCLIP, id, t0, errors.New("x"))
	cnt, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 1, cnt)
	cnt, _ = readBackfillFailure(t, db, backfillOCR, id)
	require.Equal(t, 0, cnt, "OCR 台账不该被 CLIP 失败写入")
}

// TestClearBackfillFailure_RemovesRow 验证补跑成功后台账被清掉,
// 资产立即恢复可补状态(不受此前失败的冷却影响)。
func TestClearBackfillFailure_RemovesRow(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	t0 := time.Unix(1_700_000_000, 0)

	recordBackfillFailure(db, backfillCLIP, id, t0, errors.New("x"))
	clearBackfillFailure(db, backfillCLIP, id)

	cnt, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 0, cnt)
}

// readBackfillFailure 读回台账行;无行时返回 (0, 0)。
func readBackfillFailure(t *testing.T, db *sql.DB, kind backfillKind, assetID string) (int, int64) {
	t.Helper()
	var cnt int
	var next int64
	err := db.QueryRow(
		`SELECT fail_count, next_retry_at FROM backfill_failures WHERE kind=? AND asset_id=?`,
		string(kind), assetID,
	).Scan(&cnt, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0
	}
	require.NoError(t, err)
	return cnt, next
}
