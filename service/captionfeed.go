package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"go.uber.org/zap"
)

// captionDeleteSem is a package-level concurrency semaphore (capacity 4) that
// limits how many DeleteRemote goroutines can be in flight at once: bulk
// delete / empty-trash can trigger hundreds or thousands of calls in an
// instant, and without throttling that would fire an equal number of
// concurrent HTTP requests that would knock Parser over. Package-level rather
// than per-CaptionFeeder instance because production only ever runs one
// feeder instance; a package variable keeps this in line with the other
// best-effort helper methods (FeedOne etc.) and avoids adding yet another
// field to the struct.
var captionDeleteSem = make(chan struct{}, 4)

// deleteTimeout caps a single DeleteAsset call: delete must be a "fire and
// move on" side operation — an occasional slow request must not hold a
// semaphore slot and stall subsequent deletes.
const deleteTimeout = 3 * time.Second

// captionSink is the duck-typed interface CaptionFeeder depends on, covering
// only the two methods it needs from parserclient.Client, so tests can inject
// fakes like recordingSink.
type captionSink interface {
	IngestAsset(ctx context.Context, assetID, imagePath, mime, takenAt, place string) error
	DeleteAsset(ctx context.Context, assetID string) error
}

// CaptionFeeder feeds already-indexed assets to Parser to generate captions
// (photo knowledge-base sub-project 2). Everything is best-effort: failures
// never affect the indexing/delete main flow; feeding is silently skipped
// when Parser isn't deployed.
//
// Deliberately not registered with TaskRegistry: feeding itself is
// millisecond-scale, the real slow part is Parser-side digestion (roughly 35s
// per image). Putting it on the task bar would give a false sense of
// completion — "done in a second, but not actually searchable for another
// half hour" — which is negative feedback for the user rather than useful
// information.
type CaptionFeeder struct {
	db       *sql.DB
	sink     captionSink
	thumbDir string
	running  atomic.Bool
	rerun    atomic.Bool
}

// NewCaptionFeeder constructs a CaptionFeeder. sink is usually
// parserclient.New(cfg.RuntimePath); thumbDir shares the same thumbnail
// directory as Indexer (large.jpg is used as the feed image).
func NewCaptionFeeder(db *sql.DB, sink captionSink, thumbDir string) *CaptionFeeder {
	return &CaptionFeeder{db: db, sink: sink, thumbDir: thumbDir}
}

// feedInfo looks up the mime type / taken-at / place text Parser needs for
// feeding. place is "City, Country"; if either side is empty only the
// existing part is kept.
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

// joinPlace joins city/country into "City, Country"; if either is empty only
// the existing part is kept.
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

// captionSynced looks up an asset's current caption_synced flag, used by
// FeedOne for its short-circuit check.
func (f *CaptionFeeder) captionSynced(ctx context.Context, assetID string) (bool, error) {
	var synced int
	err := f.db.QueryRowContext(ctx, `SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&synced)
	if err != nil {
		return false, err
	}
	return synced == 1, nil
}

// FeedOne feeds a single asset to Parser: look up payload → feed → on
// success set caption_synced=1. Called by the indexing inline hook
// (SetOnIndexed). No failure here ever affects the caller — this method
// never returns an error, it only logs when something is worth recording;
// ErrParserUnavailable is completely silent (Parser not being deployed is a
// normal state, it must not spam logs).
//
// caption_synced is checked before feeding: if it's already 1, return
// silently (zero logs) to keep ForceReprocess/rebuild/CLIP catch-up runs and
// other forced-rerun paths from re-burning 35s of VLM time on an asset
// that's already been handed off. When the asset's content actually changes
// (checksum changes), UPSERT already resets the flag to 0, so this
// short-circuit doesn't affect that case. A lookup failure keeps the
// existing Debug-level semantics (benign race, not worth a Warn).
func (f *CaptionFeeder) FeedOne(ctx context.Context, assetID string) {
	if synced, err := f.captionSynced(ctx, assetID); err != nil {
		zap.L().Debug("caption feed: failed to query asset info", zap.String("asset_id", assetID), zap.Error(err))
		return
	} else if synced {
		return
	}
	mime, takenAt, place, err := f.feedInfo(ctx, assetID)
	if err != nil {
		// The asset being deleted/soft-deleted between triggering the feed and
		// querying it is a benign race (e.g. the user deleted this photo at
		// almost the same time) — not worth a Warn, Debug is enough of a trace.
		zap.L().Debug("caption feed: failed to query asset info", zap.String("asset_id", assetID), zap.Error(err))
		return
	}
	imagePath := filepath.Join(f.thumbDir, assetID, "large.jpg")
	if err := f.sink.IngestAsset(ctx, assetID, imagePath, mime, takenAt, place); err != nil {
		if errors.Is(err, parserclient.ErrParserUnavailable) {
			return
		}
		zap.L().Warn("caption feed failed", zap.String("asset_id", assetID), zap.Error(err))
		return
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE assets SET caption_synced=1 WHERE id=?`, assetID); err != nil {
		zap.L().Warn("failed to set caption_synced", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// DeleteRemote asynchronously tells Parser to delete this asset's caption
// chunk, called along the full delete/trash path (Task 4): it prevents the
// agent from later matching a photo in search that no longer exists,
// producing a ghost result. Fire-and-forget — the caller
// (TrashAsset/PurgeAsset/RemoveByPath/…) neither waits for nor cares about
// the result; best-effort: package-level semaphore caps concurrency at 4,
// 3s timeout inside the goroutine; ErrParserUnavailable (Parser not
// deployed, the common case) is completely silent, any other failure only
// gets a single Warn as a trace.
func (f *CaptionFeeder) DeleteRemote(assetID string) {
	go func() {
		captionDeleteSem <- struct{}{}
		defer func() { <-captionDeleteSem }()

		ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		if err := f.sink.DeleteAsset(ctx, assetID); err != nil {
			if errors.Is(err, parserclient.ErrParserUnavailable) {
				return
			}
			zap.L().Warn("caption delete failed", zap.String("asset_id", assetID), zap.Error(err))
		}
	}()
}

// OnRestore resets an asset's caption_synced back to 0, called by the trash
// restore flow (used by Task 4): a restored asset needs to be fed again, and
// the next Backfill round will naturally pick it up.
func (f *CaptionFeeder) OnRestore(assetID string) {
	if _, err := f.db.Exec(`UPDATE assets SET caption_synced=0 WHERE id=?`, assetID); err != nil {
		zap.L().Warn("failed to reset caption_synced", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// queryPending lists asset ids pending feed: indexed, not soft-deleted,
// source file readable (not on an offline drive), not yet synced.
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

// Backfill re-feeds all under-fed indexed assets. The CAS+rerunPending
// skeleton mirrors Embedder.BackfillOCR: safe under concurrent calls, a
// second call returns nil immediately but sets the rerun flag, and the round
// already in progress automatically runs one more round (re-querying
// targets) once it finishes.
//
// It probes availability first: if the first real sink call of this round
// hits ErrParserUnavailable, that means Parser isn't deployed, so it returns
// silently for the whole round right away (no logs at all, no continuing to
// query further assets) — this is the common case (most machines don't have
// Parser installed) and every backfill sweep must not spam the log. The
// short-circuit anchor is "the first real call to sink", not list index 0 —
// see the comment inside feedBatch for details.
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

// backfillOnce is the body of a single Backfill round, without the
// concurrency dedup and rerun loop.
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

// feedBatch feeds a given list of ids one by one. Split out from
// backfillOnce so tests can inject ids directly (including nonexistent ids,
// to simulate feedInfo being deleted out from under it by an asset race)
// without depending on real concurrent timing.
//
// The short-circuit check is anchored on "the first real call to sink this
// round", not list index 0: if feedInfo fails for the first few ids (a
// benign race such as the asset being concurrently deleted/soft-deleted —
// see the continue branch below), the real ErrParserUnavailable hit may
// happen at a later index — this must still be treated as "Parser isn't
// deployed" and short-circuit the whole round silently. Missing this because
// the index isn't 0 would print a summary log on machines where Parser
// isn't deployed, defeating the "zero logs" requirement.
//
// If Unavailable is hit on a call that isn't the first, that means Parser
// was available at the start of this round and only went offline partway
// through (e.g. the Parser container was restarted mid-sweep) — this is a
// normal ops scenario, so the loop is broken to avoid retrying a Parser
// that's known to be down one by one, but the summary log is kept (real
// feeding already happened, worth recording — this is not the "zero
// deployment" silent case).
func (f *CaptionFeeder) feedBatch(ctx context.Context, ids []string) error {
	var fed, failed int64
	firstSinkCall := true
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		mime, takenAt, place, ierr := f.feedInfo(ctx, id)
		if ierr != nil {
			// The asset being deleted/soft-deleted between queryPending
			// selecting it and here is a benign race; it doesn't count as a
			// sink attempt and isn't enough to judge whether Parser is
			// deployed — count it in failed and move on to the next one.
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
					// Unavailable on the first real sink call of this round:
					// Parser isn't deployed, short-circuit the whole round
					// silently, no summary log.
					return nil
				}
				// Unavailable on a call that isn't the first: Parser went
				// offline partway through — break the loop but keep the
				// summary log (normal ops scenario, worth recording).
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
	zap.L().Info("caption backfill sweep complete", zap.Int64("fed", fed), zap.Int64("failed", failed))
	return nil
}
