package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// recordingSink is a captionSink fake for test injection: it records the full
// payload of every call, and can inject failWith to simulate a feed failure
// (including the ErrParserUnavailable silent scenario).
type recordingSink struct {
	mu          sync.Mutex
	ingests     []string
	deletes     []string
	deleteCalls int   // counted regardless of success/failure, so tests can tell whether DeleteAsset was ever called (in the fire-and-forget path, deletes isn't appended to when a failure is injected)
	failWith    error // injects ErrParserUnavailable / a generic error
}

func (r *recordingSink) IngestAsset(_ context.Context, id, path, mime, takenAt, place string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.ingests = append(r.ingests, id+"|"+path+"|"+mime+"|"+takenAt+"|"+place)
	return nil
}

func (r *recordingSink) DeleteAsset(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls++
	if r.failWith != nil {
		return r.failWith
	}
	r.deletes = append(r.deletes, id)
	return nil
}

// sequenceSink consumes a preset list of errors in call order (nil means
// success), used for tests that need to distinguish "which call number to
// sink" scenarios (e.g. the first feedInfo fails and the call that actually
// hits ErrParserUnavailable is really the second sink call).
type sequenceSink struct {
	mu      sync.Mutex
	results []error
	calls   []string
}

func (s *sequenceSink) IngestAsset(_ context.Context, id, _, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.calls)
	s.calls = append(s.calls, id)
	if idx < len(s.results) {
		return s.results[idx]
	}
	return nil
}

func (s *sequenceSink) DeleteAsset(context.Context, string) error { return nil }

// insertCaptionCandidate inserts an asset that Backfill will select: indexed,
// not soft-deleted, not on an offline drive, caption_synced=0.
func insertCaptionCandidate(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status, caption_synced)
		VALUES(?,?,?,'indexed',0)`, id, "/g/"+id+".jpg", "image/jpeg")
	require.NoError(t, err)
}

// FeedOne: after a successful feed, the payload contains the large.jpg
// path/mime/taken_at/geo place, and synced is set to 1.
func TestFeedOnePayloadAndMark(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	assetID := "a1"

	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, taken_at, status, caption_synced)
		VALUES(?,?,?,?,'indexed',0)`, assetID, "/g/a1.jpg", "image/jpeg", "2024-05-01 12:00:00")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, assetID, "Shanghai", "China")
	require.NoError(t, err)

	sink := &recordingSink{}
	f := NewCaptionFeeder(db, sink, thumbDir)
	f.FeedOne(context.Background(), assetID)

	wantPath := filepath.Join(thumbDir, assetID, "large.jpg")
	sink.mu.Lock()
	got := append([]string(nil), sink.ingests...)
	sink.mu.Unlock()
	require.Equal(t, []string{assetID + "|" + wantPath + "|image/jpeg|2024-05-01|Shanghai, China"}, got)

	var synced int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&synced))
	require.Equal(t, 1, synced)
}

// FeedOne: when sink fails, synced stays 0; a generic error produces a Warn
// log, ErrParserUnavailable is completely silent.
func TestFeedOneFailureLeavesUnsynced(t *testing.T) {
	t.Run("generic error is logged", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertIndexedAsset(t, db, "a1")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: errors.New("boom")}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), "a1")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.NotEmpty(t, logs.All(), "a generic error should produce a log trace")
	})

	t.Run("ErrParserUnavailable is completely silent", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertIndexedAsset(t, db, "a1")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: parserclient.ErrParserUnavailable}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), "a1")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.Empty(t, logs.All(), "ErrParserUnavailable should not produce any log")
	})
}

// FeedOne: caption_synced=1 short-circuits — an already-handed-off asset must
// not call sink again (guards against ForceReprocess/rebuild/CLIP catch-up
// runs and other forced-rerun paths re-burning 35s of VLM time); synced=0
// still feeds as normal.
func TestFeedOneSyncedShortCircuit(t *testing.T) {
	t.Run("already synced does not feed again", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		assetID := "a1"
		_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status, caption_synced)
			VALUES(?,?,?,'indexed',1)`, assetID, "/g/a1.jpg", "image/jpeg")
		require.NoError(t, err)

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), assetID)

		sink.mu.Lock()
		got := append([]string(nil), sink.ingests...)
		sink.mu.Unlock()
		require.Empty(t, got, "an already-synced asset should not call sink again")
	})

	t.Run("unsynced asset feeds as normal", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertIndexedAsset(t, db, "a1")

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), "a1")

		sink.mu.Lock()
		got := append([]string(nil), sink.ingests...)
		sink.mu.Unlock()
		require.Len(t, got, 1, "an unsynced asset should feed as normal")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&synced))
		require.Equal(t, 1, synced)
	})
}

// Backfill: only feeds visible assets with synced=0; sets 1 on each success;
// CAS guards against reentrancy; short-circuits the whole round silently
// when the first asset hits ErrParserUnavailable.
func TestBackfillSelectionAndCAS(t *testing.T) {
	t.Run("correct selection and per-asset marking", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()

		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")
		// Not selected: already synced.
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced) VALUES('s1','/g/s1.jpg','indexed',1)`)
		require.NoError(t, err)
		// Not selected: not yet fully indexed.
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced) VALUES('p1','/g/p1.jpg','pending',0)`)
		require.NoError(t, err)
		// Not selected: soft-deleted.
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced,deleted_at) VALUES('d1','/g/d1.jpg','indexed',0,CURRENT_TIMESTAMP)`)
		require.NoError(t, err)
		// Not selected: source file is on an offline drive.
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced,offline) VALUES('o1','/g/o1.jpg','indexed',0,1)`)
		require.NoError(t, err)

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.Backfill(context.Background()))

		sink.mu.Lock()
		var gotIDs []string
		for _, s := range sink.ingests {
			gotIDs = append(gotIDs, strings.SplitN(s, "|", 2)[0])
		}
		sink.mu.Unlock()
		require.ElementsMatch(t, []string{"e1", "e2"}, gotIDs)

		for _, id := range []string{"e1", "e2"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 1, synced, "a selected asset should be set to synced=1")
		}
		for _, id := range []string{"p1", "d1", "o1"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 0, synced, "a non-selected asset should not be touched")
		}
	})

	t.Run("first asset hitting ErrParserUnavailable short-circuits the whole round silently", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: parserclient.ErrParserUnavailable}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.Backfill(context.Background()))

		sink.mu.Lock()
		require.Empty(t, sink.ingests)
		sink.mu.Unlock()
		for _, id := range []string{"e1", "e2"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 0, synced)
		}
		require.Empty(t, logs.All(), "the whole round should be silent when Parser isn't deployed, no logs")
	})

	t.Run("concurrent calls are CAS-safe", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)

		done := make(chan struct{}, 2)
		for i := 0; i < 2; i++ {
			go func() {
				defer func() { done <- struct{}{} }()
				require.NoError(t, f.Backfill(context.Background()))
			}()
		}
		<-done
		<-done

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e1'`).Scan(&synced))
		require.Equal(t, 1, synced)
	})

	// Regression test: the short-circuit check used to be hard-wired to index
	// 0. If the first entry's feedInfo fails (a benign race such as the asset
	// being concurrently deleted), the real ErrParserUnavailable hit is at
	// index 1, so the short-circuit would fail to fire and a summary Info
	// would still be printed after a whole round of empty iteration —
	// violating the "zero logs on an undeployed machine" requirement. Uses
	// feedBatch to directly inject a nonexistent id up front to simulate this
	// race.
	t.Run("a failed feedInfo on the first entry does not mask the first-sink short-circuit", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &sequenceSink{results: []error{parserclient.ErrParserUnavailable}}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.feedBatch(context.Background(), []string{"ghost", "e2"}))

		sink.mu.Lock()
		gotCalls := append([]string(nil), sink.calls...)
		sink.mu.Unlock()
		require.Equal(t, []string{"e2"}, gotCalls, "sink should only be really called once (ghost's feedInfo failure doesn't count as a sink attempt)")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e2'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.Empty(t, logs.All(), "the whole round should be silent when Parser isn't deployed, no summary log")
	})

	// Unavailable hit on a call that isn't the first: Parser was available at
	// one point (e1 fed successfully) and only went offline partway through
	// this round — a normal ops scenario, so the loop should break but the
	// summary log should be kept.
	t.Run("a non-first Unavailable hit keeps the summary log", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.InfoLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &sequenceSink{results: []error{nil, parserclient.ErrParserUnavailable}}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.feedBatch(context.Background(), []string{"e1", "e2"}))

		sink.mu.Lock()
		gotCalls := append([]string(nil), sink.calls...)
		sink.mu.Unlock()
		require.Equal(t, []string{"e1", "e2"}, gotCalls)

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e1'`).Scan(&synced))
		require.Equal(t, 1, synced, "e1 fed successfully so synced should be set to 1")
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e2'`).Scan(&synced))
		require.Equal(t, 0, synced, "e2 hit Unavailable so it should not be marked")

		entries := logs.All()
		require.Len(t, entries, 1, "going offline mid-round is a normal ops scenario, the summary log should be kept")
		require.Equal(t, "caption backfill sweep complete", entries[0].Message)
	})
}

// SetOnIndexed: once the indexing pipeline writes an asset as
// status='indexed', the hook should be called asynchronously exactly once,
// carrying the correct asset id.
func TestOnIndexedHookFires(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	ix := NewIndexer(db, &mockML{}, thumbDir, 1)

	var mu sync.Mutex
	var got []string
	done := make(chan struct{}, 1)
	ix.SetOnIndexed(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		done <- struct{}{}
	})

	srcDir := t.TempDir()
	path := makeTestJPEG(t, srcDir)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onIndexed hook did not fire within the timeout")
	}

	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&id))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{id}, got)
}

// concurrencySink is a fake sink for testing DeleteRemote's concurrency cap:
// DeleteAsset blocks on release until the test lets it go, while recording
// the peak in-flight concurrency, used to assert the package-level semaphore
// (capacity 4) is actually in effect.
type concurrencySink struct {
	mu         sync.Mutex
	current    int
	maxCurrent int
	release    chan struct{}
}

func (s *concurrencySink) IngestAsset(context.Context, string, string, string, string, string) error {
	return nil
}

func (s *concurrencySink) DeleteAsset(_ context.Context, _ string) error {
	s.mu.Lock()
	s.current++
	if s.current > s.maxCurrent {
		s.maxCurrent = s.current
	}
	s.mu.Unlock()

	<-s.release

	s.mu.Lock()
	s.current--
	s.mu.Unlock()
	return nil
}

func (s *concurrencySink) snapshot() (current, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.maxCurrent
}

// DeleteRemote: the concurrency cap is locked at 4 — firing off 10
// DeleteRemote calls at once, the number of in-flight DeleteAsset calls
// should climb to 4 and go no further (the package-level semaphore is in
// effect), and drop back to zero once everything is released (no goroutine
// leak/deadlock).
func TestDeleteRemoteConcurrencyLimit(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	sink := &concurrencySink{release: make(chan struct{})}
	f := NewCaptionFeeder(db, sink, thumbDir)

	const n = 10
	for i := 0; i < n; i++ {
		f.DeleteRemote(fmt.Sprintf("a%d", i))
	}

	require.Eventually(t, func() bool {
		cur, _ := sink.snapshot()
		return cur == 4
	}, 2*time.Second, 10*time.Millisecond, "concurrency should climb to the semaphore limit of 4")

	// Hold at the limit for a short while to confirm the 5th call and beyond
	// really are stuck, rather than accidentally letting a few more through.
	time.Sleep(100 * time.Millisecond)
	cur, max := sink.snapshot()
	require.Equal(t, 4, cur, "before everything is released, in-flight calls should not exceed the semaphore capacity")
	require.Equal(t, 4, max, "10 concurrent requests should saturate the semaphore at 4")

	close(sink.release)
	require.Eventually(t, func() bool {
		cur, _ := sink.snapshot()
		return cur == 0
	}, 2*time.Second, 10*time.Millisecond, "in-flight calls should drop to zero once everything is released")
}

// DeleteRemote: ErrParserUnavailable is completely silent (Parser not being
// deployed is the common case); a generic error produces exactly one Warn as
// a trace — both are fire-and-forget and don't affect the caller.
func TestDeleteRemoteFailureSemantics(t *testing.T) {
	t.Run("ErrParserUnavailable is silent", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: parserclient.ErrParserUnavailable}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.DeleteRemote("a1")

		require.Eventually(t, func() bool {
			sink.mu.Lock()
			defer sink.mu.Unlock()
			return sink.deleteCalls == 1
		}, 2*time.Second, 10*time.Millisecond, "DeleteRemote should call sink asynchronously exactly once")
		// After the sink call, the goroutine has only one errors.Is check left
		// before returning; leave a short margin for it to finish, then
		// confirm zero logs the whole way through.
		time.Sleep(50 * time.Millisecond)
		require.Empty(t, logs.All(), "ErrParserUnavailable should not produce any log")
	})

	t.Run("a generic error produces exactly one Warn", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: errors.New("boom")}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.DeleteRemote("a1")

		require.Eventually(t, func() bool {
			return len(logs.All()) >= 1
		}, 2*time.Second, 10*time.Millisecond, "a generic error should produce a Warn log")
		require.Len(t, logs.All(), 1, "a generic error should produce exactly one Warn log")
	})
}
