package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	sqlite3 "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// captionLister is the minimal interface Puller depends on, taking only the
// ListCaptions method so tests can inject a fake (see captionpull_test.go's
// fakeLister) without depending directly on the concrete parserclient.Client
// type.
type captionLister interface {
	ListCaptions(ctx context.Context, offset string) ([]parserclient.CaptionItem, string, error)
}

// captionDeleter is the minimal interface for orphan cleanup (satisfied by
// parserclient.Client).
type captionDeleter interface {
	DeleteAsset(ctx context.Context, assetID string) error
}

// Puller periodically pulls the full caption set from NimoOS-Parser and
// diff-upserts it into the local asset_caption table (the flow-back side of
// photo knowledge-base sub-project 2; caption consumption/retrieval is a
// later sub-project — this package is only responsible for landing the data
// locally).
//
// Everything is best-effort: Parser not deployed / network failure / 503
// (qdrant unavailable) all just make this round's PullOnce return err
// directly; the caller's hook only logs it, never propagates it upward, and
// it never affects the Photos indexing/search main flow.
//
// Lifecycle is self-consistent:
//   - Parser-side caption update (regenerated) → mtime_ms increases, and the
//     next flow-back round overwrites the old local text accordingly; if
//     mtime hasn't changed it's skipped, avoiding a pointless write.
//   - Asset deleted → asset_caption is automatically cascade-cleaned via the
//     asset_id foreign key's ON DELETE CASCADE, this package doesn't need to
//     care.
//   - Asset restored (e.g. from trash) → as long as the asset row still
//     exists (cascade never fired), the old caption row is kept as-is; if
//     Parser has an update later, the mtime overwrite naturally takes effect.
//   - Orphan (Parser already generated a caption, but the local assets table
//     has no such id yet — e.g. the two sides' delete notifications are out
//     of sync in time, or the delete notification is fire-and-forget and
//     Parser was unreachable at the time, causing a permanent missed
//     delete) → the INSERT fails under the foreign key constraint, so it's
//     skipped and processing continues with the next item without
//     interrupting the whole pull; it also best-effort deletes the vector on
//     Parser's side as reconciliation backstop for that miss.
type Puller struct {
	db      *sql.DB
	lister  captionLister
	deleter captionDeleter
}

// NewPuller constructs a Puller. lister is usually
// parserclient.New(cfg.RuntimePath) (sharing the same parserclient.Client
// instance as CaptionFeeder is fine — ListCaptions and IngestAsset/
// DeleteAsset go through the same discoveryFile/http.Client). deleter is used
// for orphan cleanup and may be nil (degrades to skip-only, no cleanup).
func NewPuller(db *sql.DB, lister captionLister, deleter captionDeleter) *Puller {
	return &Puller{db: db, lister: lister, deleter: deleter}
}

// localMtime looks up the mtime_ms currently recorded for an asset in the
// local asset_caption table; ok=false if there's no record yet.
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

// PullOnce pulls the full set of Parser-side captions and diff-upserts them
// into the local table: loops the pagination cursor to the end, comparing
// each item against the local mtime_ms, writing only when the local record is
// missing or Parser's mtime is larger (ON CONFLICT overwrite); on write
// failure it distinguishes precisely: only SQLITE_CONSTRAINT_FOREIGNKEY (a
// genuine orphan asset) is skipped and continues, any other error (SQLITE_BUSY
// timeout, disk I/O, other real failures) makes the whole round return err
// directly, avoiding misfiling a real failure as an orphan and silently
// swallowing it.
//
// A lister error (Parser not deployed / network failure / non-2xx) likewise
// makes the whole round return err directly, with the upserted count so far
// returned as-is — the caller (the hook) follows best-effort semantics and
// only logs it, never propagating a fatal error upward.
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
				continue // local is already the same version or newer, skip to avoid a pointless write
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
					// Genuine orphan: local assets has no such id. Besides
					// skipping, also delete the vector on Parser's side —
					// the delete notification is fire-and-forget
					// (captionfeed.go DeleteRemote), and Parser being
					// unreachable at the time causes a permanent missed
					// delete; this is the only reconciliation backstop for
					// that. best-effort: failure only gets a Warn, doesn't
					// interrupt the round.
					zap.L().Debug("caption pull: write skipped (orphan asset, foreign key constraint failed)",
						zap.String("asset_id", it.AssetID), zap.Error(werr))
					if p.deleter != nil {
						if derr := p.deleter.DeleteAsset(ctx, it.AssetID); derr != nil && !errors.Is(derr, parserclient.ErrParserUnavailable) {
							zap.L().Warn("caption pull: orphan vector cleanup failed", zap.String("asset_id", it.AssetID), zap.Error(derr))
						}
					}
					continue
				}
				// A non-foreign-key error (SQLITE_BUSY timeout, disk I/O,
				// other real failures) must not be silently swallowed as an
				// orphan — that would erase the failure signal. Return err
				// directly for the whole round and let the hook's existing
				// Warn-logging path handle it.
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
