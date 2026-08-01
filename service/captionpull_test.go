// Tests for Puller: injects pagination/error scenarios via fakeLister to
// verify diff-upsert semantics.
package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"github.com/stretchr/testify/require"
)

// fakeLister is a captionLister fake for test injection: returns a preset
// page keyed by offset, or directly returns the injected error (simulating
// Parser not deployed / network failure / 503).
type fakeLister struct {
	pages map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}
	err error
}

func (f *fakeLister) ListCaptions(_ context.Context, offset string) ([]parserclient.CaptionItem, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	p := f.pages[offset]
	return p.items, p.next, nil
}

// fakeDeleter is a captionDeleter fake for test injection, recording the
// asset_ids that were cleaned up.
type fakeDeleter struct{ deleted []string }

func (f *fakeDeleter) DeleteAsset(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// insertCaptionAsset inserts an asset row that asset_caption's foreign key
// will reference (only the id needs to exist, other fields are irrelevant to
// this test).
func insertCaptionAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?,'indexed')`, id, "/g/"+id+".jpg")
	require.NoError(t, err)
}

// PullOnce should page through the full set and upsert each item into the
// local table.
func TestPullOnce_PagesAndUpserts(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")
	insertCaptionAsset(t, db, "a2")

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"":   {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "一只猫", MtimeMs: 100}}, next: "c2"},
		"c2": {items: []parserclient.CaptionItem{{AssetID: "a2", Text: "一片海", MtimeMs: 200}}, next: ""},
	}}

	p := NewPuller(db, lister, nil)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)

	var text string
	var mtime int64
	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "一只猫", text)
	require.Equal(t, int64(100), mtime)

	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a2'`).Scan(&text, &mtime))
	require.Equal(t, "一片海", text)
	require.Equal(t, int64(200), mtime)
}

// PullOnce skips overwriting records whose mtime hasn't changed, and
// overwrites records whose mtime increased.
func TestPullOnce_SkipsUnchangedUpdatesChanged(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")
	_, err := db.Exec(`INSERT INTO asset_caption(asset_id, text, mtime_ms) VALUES('a1','旧文本',5)`)
	require.NoError(t, err)

	// Round 1: same mtime (5), different text → should not overwrite.
	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "新文本-未变", MtimeMs: 5}}, next: ""},
	}}
	p := NewPuller(db, lister, nil)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "an unchanged mtime should not count toward upserted")

	var text string
	var mtime int64
	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "旧文本", text, "an unchanged mtime should keep the old text")
	require.Equal(t, int64(5), mtime)

	// Round 2: mtime increased (9) → should overwrite.
	lister2 := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "新文本-已变", MtimeMs: 9}}, next: ""},
	}}
	p2 := NewPuller(db, lister2, nil)
	n2, err := p2.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n2, "an increased mtime should count toward upserted")

	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "新文本-已变", text)
	require.Equal(t, int64(9), mtime)
}

// PullOnce: when lister errors it returns err but doesn't panic, and the
// local table is unaffected (the caller's hook only logs it).
func TestPullOnce_ListerErrorSilent(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")

	lister := &fakeLister{err: errors.New("parser 503")}
	p := NewPuller(db, lister, nil)
	n, err := p.PullOnce(context.Background())
	require.Error(t, err)
	require.Equal(t, 0, n)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption`).Scan(&count))
	require.Equal(t, 0, count, "the local table should have no changes when lister errors")
}

// PullOnce: a "non-foreign-key" error on write (simulating a disk I/O/
// constraint real failure via a trigger, as distinct from an orphan asset's
// foreign key constraint failure) should return err directly for the whole
// round, and must not be silently treated as an orphan continue — otherwise
// a real failure like an SQLITE_BUSY timeout would get erased into an
// "orphan skip".
//
// Uses a BEFORE INSERT trigger to actively RAISE(ABORT,...) for a specific
// asset_id, producing a write failure that is a "sqlite3.Error but
// ExtendedCode isn't ErrConstraintForeignKey", precisely verifying that
// PullOnce judges by ExtendedCode rather than "treat any Exec error as an
// orphan".
func TestPullOnce_NonForeignKeyErrorPropagates(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "boom") // the asset genuinely exists, it's not an orphan

	_, err := db.Exec(`
		CREATE TRIGGER trg_force_fail BEFORE INSERT ON asset_caption
		WHEN NEW.asset_id = 'boom'
		BEGIN
			SELECT RAISE(ABORT, 'forced non-fk failure for test');
		END;`)
	require.NoError(t, err)

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "boom", Text: "x", MtimeMs: 1}}, next: ""},
	}}
	p := NewPuller(db, lister, nil)
	n, err := p.PullOnce(context.Background())
	require.Error(t, err, "a non-foreign-key write failure should propagate as err, not be silently swallowed")
	require.Equal(t, 0, n)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption WHERE asset_id='boom'`).Scan(&count))
	require.Equal(t, 0, count, "a write failure should not leave a half-finished record")
}

// PullOnce: an orphan asset_id that doesn't exist in local assets should be
// skipped and processing continues, without affecting the rest of the items
// being written.
func TestPullOnce_OrphanSkipped(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a2") // only a2 is inserted, a1 is an orphan

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{
			{AssetID: "a1", Text: "孤儿", MtimeMs: 1},
			{AssetID: "a2", Text: "正常", MtimeMs: 2},
		}, next: ""},
	}}
	p := NewPuller(db, lister, nil)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "the orphan should be skipped, only a2 counts toward upserted")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption WHERE asset_id='a1'`).Scan(&count))
	require.Equal(t, 0, count, "the orphan should not be written")

	var text string
	require.NoError(t, db.QueryRow(`SELECT text FROM asset_caption WHERE asset_id='a2'`).Scan(&text))
	require.Equal(t, "正常", text)
}

// PullOnce, when it hits an orphan (local assets has no such id), should
// best-effort delete the vector on Parser's side — the delete notification is
// fire-and-forget, and Parser being unreachable at the time causes a
// permanent missed delete; this is the only reconciliation backstop.
// A cleanup failure must not affect this round's pull (doesn't count as err,
// doesn't block other items).
func TestPullOnceDeletesOrphanRemote(t *testing.T) {
	db := makeTestDB(t) // no assets row is inserted for "ghost", it's a genuine orphan

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "ghost", Text: "孤儿", MtimeMs: 1}}, next: ""},
	}}
	deleter := &fakeDeleter{}
	p := NewPuller(db, lister, deleter)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, []string{"ghost"}, deleter.deleted)
}
