package service

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// readBackfillFailure returns (fail_count, next_retry_at) or (0, 0) when absent.
func readBackfillFailure(t *testing.T, db *sql.DB, kind backfillKind, assetID string) (int, int64) {
	t.Helper()
	var n int
	var next int64
	err := db.QueryRow(`SELECT fail_count, next_retry_at FROM backfill_failures WHERE kind=? AND asset_id=?`,
		string(kind), assetID).Scan(&n, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0
	}
	require.NoError(t, err)
	return n, next
}

func TestBackfillRetryDelay_Escalates(t *testing.T) {
	require.Equal(t, 5*time.Minute, backfillRetryDelay(1))
	require.Equal(t, 15*time.Minute, backfillRetryDelay(2))
	require.Equal(t, time.Hour, backfillRetryDelay(3))
	require.Equal(t, 6*time.Hour, backfillRetryDelay(4))
	require.Equal(t, 24*time.Hour, backfillRetryDelay(5))
	require.Equal(t, 24*time.Hour, backfillRetryDelay(99)) // clamped at the cap
	require.Equal(t, 5*time.Minute, backfillRetryDelay(0)) // defensive clamp
}

// Proves the SQL CASE ladder matches the Go ladder — the one desync that
// would silently break escalation.
func TestRecordBackfillFailure_AccumulatesAndSchedules(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	t0 := time.Now()
	recordBackfillFailure(db, backfillCLIP, id, t0, errors.New("boom"))
	n, next := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 1, n)
	require.Equal(t, t0.UnixMilli()+(5*time.Minute).Milliseconds(), next)

	t1 := t0.Add(10 * time.Minute)
	recordBackfillFailure(db, backfillCLIP, id, t1, errors.New("boom again"))
	n, next = readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 2, n)
	require.Equal(t, t1.UnixMilli()+(15*time.Minute).Milliseconds(), next)
}

// TestRecordBackfillFailure_FullLadderIncludingClamp exercises every rung of
// the escalation ladder — including one step past the cap — proving the SQL
// CASE ladder tracks backfillRetryDelays at every level, not just the first
// two. This is the branch most likely to silently regress if either ladder
// (Go or SQL) is edited without the other.
func TestRecordBackfillFailure_FullLadderIncludingClamp(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")

	ts := time.Now()
	for i := 1; i <= 7; i++ {
		recordBackfillFailure(db, backfillCLIP, id, ts, errors.New("boom"))
		n, next := readBackfillFailure(t, db, backfillCLIP, id)
		require.Equal(t, i, n, "fail_count at iteration %d", i)
		require.Equal(t, ts.UnixMilli()+backfillRetryDelay(i).Milliseconds(), next,
			"next_retry_at at iteration %d", i)
		ts = ts.Add(time.Minute)
	}
}

func TestRecordBackfillFailure_KindsAreIndependent(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	recordBackfillFailure(db, backfillCLIP, id, time.Now(), errors.New("x"))
	n, _ := readBackfillFailure(t, db, backfillOCR, id)
	require.Zero(t, n)
}

func TestClearBackfillFailure_RemovesRow(t *testing.T) {
	db := makeTestDB(t)
	id := insertAsset(t, db, "/a.jpg", "indexed")
	recordBackfillFailure(db, backfillCLIP, id, time.Now(), errors.New("x"))
	clearBackfillFailure(db, backfillCLIP, id)
	n, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Zero(t, n)
}
