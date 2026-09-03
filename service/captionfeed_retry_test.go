package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

func seedHandedOff(t *testing.T, id string, handedAgo time.Duration, attempts int, landed bool) (*CaptionFeeder, *recordingSink) {
	t.Helper()
	db := makeTestDB(t)
	handedAt := time.Now().Add(-handedAgo).UnixMilli()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status, caption_synced, caption_handed_at, caption_attempts)
		VALUES(?,?,?,'indexed',1,?,?)`, id, "/g/"+id+".jpg", "image/jpeg", handedAt, attempts)
	require.NoError(t, err)
	if landed {
		_, err = db.Exec(`INSERT INTO asset_caption(asset_id, text, mtime_ms) VALUES(?, 'a cat', 1)`, id)
		require.NoError(t, err)
	}
	sink := &recordingSink{}
	return NewCaptionFeeder(db, sink, t.TempDir()), sink
}

func ingestCount(s *recordingSink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ingests)
}

// Parser answered 202 (job queued) a day ago but no caption ever landed:
// the job failed permanently on Parser's side (5 attempts, no callback).
// Backfill must hand the asset off again instead of trusting the flag forever.
func TestBackfill_RefeedsHandedOffAssetWhoseCaptionNeverLanded(t *testing.T) {
	f, sink := seedHandedOff(t, "a1", captionStaleAfter+time.Hour, 1, false)
	require.NoError(t, f.Backfill(context.Background()))
	require.Equal(t, 1, ingestCount(sink), "stale hand-off must be re-fed")
	var attempts int
	require.NoError(t, f.db.QueryRow(`SELECT caption_attempts FROM assets WHERE id='a1'`).Scan(&attempts))
	require.Equal(t, 2, attempts)
}

func TestBackfill_LeavesLandedCaptionAlone(t *testing.T) {
	f, sink := seedHandedOff(t, "a1", captionStaleAfter+time.Hour, 1, true)
	require.NoError(t, f.Backfill(context.Background()))
	require.Equal(t, 0, ingestCount(sink), "a caption that landed is done; never re-burn VLM time on it")
}

func TestBackfill_RecentHandoffIsStillInFlight(t *testing.T) {
	f, sink := seedHandedOff(t, "a1", time.Hour, 1, false)
	require.NoError(t, f.Backfill(context.Background()))
	require.Equal(t, 0, ingestCount(sink), "Parser may simply still be working through its queue")
}

func TestBackfill_GivesUpAfterMaxAttempts(t *testing.T) {
	f, sink := seedHandedOff(t, "a1", captionStaleAfter+time.Hour, captionMaxAttempts, false)
	require.NoError(t, f.Backfill(context.Background()))
	require.Equal(t, 0, ingestCount(sink), "bounded retries: a permanently failing asset must not loop forever")
}

// Mirror image of the above: a hand-off that Parser rejects outright (e.g.
// 400 because large.jpg is missing) used to be retried on every sweep forever.
func TestBackfill_RejectedHandoffStopsAfterMaxAttempts(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionCandidate(t, db, "a1")
	sink := &recordingSink{failWith: errBoom}
	f := NewCaptionFeeder(db, sink, t.TempDir())
	for i := 0; i < captionMaxAttempts+2; i++ {
		require.NoError(t, f.Backfill(context.Background()))
	}
	var attempts, synced int
	require.NoError(t, db.QueryRow(`SELECT caption_attempts, caption_synced FROM assets WHERE id='a1'`).Scan(&attempts, &synced))
	require.Equal(t, captionMaxAttempts, attempts, "each rejected hand-off counts; sweeps after the cap must skip the asset")
	require.Equal(t, 0, synced)
}

func TestOnRestore_ResetsAttemptsSoARestoredPhotoGetsAFreshBudget(t *testing.T) {
	f, _ := seedHandedOff(t, "a1", captionStaleAfter+time.Hour, captionMaxAttempts, false)
	f.OnRestore("a1")
	var attempts, synced int
	require.NoError(t, f.db.QueryRow(`SELECT caption_attempts, caption_synced FROM assets WHERE id='a1'`).Scan(&attempts, &synced))
	require.Equal(t, 0, attempts)
	require.Equal(t, 0, synced)
}
