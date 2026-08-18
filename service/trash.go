package service

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// orphanTrashDirAge is how old an empty ".trash/<id>/" directory must be
// before CleanupOrphanTrashDirs will remove it. The floor exists so the sweep
// never races a moveToTrash that is still in flight (mkdir happens before the
// file lands in it).
const orphanTrashDirAge = time.Hour

// TrashService implements soft delete (trash): deleting moves the original
// file to <volumeRoot>/.trash/<id>/ (the volume root being whichever
// currently-mounted scan root the asset's file already lives under — see
// trashDirFor) and records deleted_at in the DB; restoring moves it back; a
// permanent delete removes the file + thumbnail + DB record.
//
// Pinning the trash directory to the asset's own volume (instead of a single
// fixed gallery root) is what keeps the move a same-device os.Rename: photo
// libraries routinely span multiple mounted filesystems (e.g. /DATA plus one
// or more /media/RAID_* arrays), and os.Rename fails with EXDEV across
// devices. See the 2026-08-18 delete-chain diagnosis for the incident this
// fixes.
type TrashService struct {
	db         *sql.DB
	galleryDir string // fallback trash root, used only when an asset's path isn't under any currently-known scan root (see trashDirFor)
	thumbDir   string // thumbnail root, cleaned up on permanent delete

	// scanRoots returns the currently-mounted volume roots to match an
	// asset's path against. Defaults to EnumerateScanRoots; overridden in
	// tests to avoid depending on the real /proc/mounts.
	scanRoots func() []string

	// osRename performs the actual same-device move. Defaults to os.Rename;
	// overridden in tests to simulate EXDEV without needing two real devices.
	osRename func(oldpath, newpath string) error

	// onCaptionDelete/onCaptionRestore are the caption hand-off hooks (Task
	// 4). Function fields avoid TrashService importing CaptionFeeder
	// directly (same injection convention as MountGuard/Embedder). Safely
	// skipped when nil (not wired up / tests).
	onCaptionDelete  func(assetID string)
	onCaptionRestore func(assetID string)
}

// NewTrashService constructs a TrashService.
func NewTrashService(db *sql.DB, galleryDir, thumbDir string) *TrashService {
	return &TrashService{
		db:         db,
		galleryDir: galleryDir,
		thumbDir:   thumbDir,
		scanRoots:  EnumerateScanRoots,
		osRename:   os.Rename,
	}
}

// SetCaptionDelete injects the caption-delete callback fired after a
// soft/permanent delete succeeds (usually CaptionFeeder.DeleteRemote).
func (s *TrashService) SetCaptionDelete(fn func(assetID string)) {
	s.onCaptionDelete = fn
}

// SetCaptionRestore injects the caption-restore callback fired after a
// restore succeeds (usually CaptionFeeder.OnRestore).
func (s *TrashService) SetCaptionRestore(fn func(assetID string)) {
	s.onCaptionRestore = fn
}

// trashDirFor returns the .trash/<id>/ directory an asset currently at
// filePath should be moved into: the .trash root under whichever
// currently-mounted scan root filePath lives on, so the subsequent
// os.Rename stays on the same device. Falls back to galleryDir when
// filePath isn't under any currently-known scan root (e.g. an unusual
// WatchDirs entry, or /proc/mounts being unreadable at the moment
// EnumerateScanRoots ran).
func (s *TrashService) trashDirFor(id, filePath string) string {
	root := VolumeRootForPath(filePath, s.scanRoots())
	if root == "" {
		root = s.galleryDir
	}
	return filepath.Join(root, ".trash", id)
}

// TrashAsset moves an asset into the trash (including its Live Photo video
// companion, if any).
func (s *TrashService) TrashAsset(id string) error {
	var filePath, liveID string
	err := s.db.QueryRow(
		`SELECT file_path, COALESCE(live_photo_video_id,'') FROM assets WHERE id=? AND deleted_at IS NULL`,
		id).Scan(&filePath, &liveID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("TrashAsset query: %w", err)
	}
	if err := s.moveToTrash(id, filePath); err != nil {
		return err
	}
	if liveID != "" {
		var livePath string
		if e := s.db.QueryRow(
			`SELECT file_path FROM assets WHERE id=? AND deleted_at IS NULL`, liveID,
		).Scan(&livePath); e == nil {
			if me := s.moveToTrash(liveID, livePath); me == nil && s.onCaptionDelete != nil {
				s.onCaptionDelete(liveID) // caption hand-off: delete the Live Photo companion asset's caption too, to avoid ghost results
			}
		}
	}
	if s.onCaptionDelete != nil {
		s.onCaptionDelete(id) // caption hand-off: prevent the agent from retrieving ghost results
	}
	return nil
}

func (s *TrashService) moveToTrash(id, filePath string) error {
	dstDir := s.trashDirFor(id, filePath)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("moveToTrash mkdir: %w", err)
	}
	dst := filepath.Join(dstDir, filepath.Base(filePath))
	// Update DB BEFORE moving the file. The fsnotify watcher fires a Rename/Remove
	// event for the old path and calls Indexer.RemoveByPath, which matches on
	// file_path. By flipping file_path to the .trash path first, that async handler
	// no longer matches the old path and cannot hard-delete this row (race fix).
	if _, err := s.db.Exec(
		`UPDATE assets SET original_path=?, file_path=?, deleted_at=CURRENT_TIMESTAMP WHERE id=?`,
		filePath, dst, id); err != nil {
		os.RemoveAll(dstDir) //nolint:errcheck  clean up the just-created empty trash dir; the DB update never landed
		return fmt.Errorf("moveToTrash update: %w", err)
	}
	if err := s.renameOrCopy(filePath, dst); err != nil {
		// Roll back so the row keeps pointing at the still-present original file.
		s.db.Exec(`UPDATE assets SET original_path=NULL, file_path=?, deleted_at=NULL WHERE id=?`, filePath, id) //nolint:errcheck
		// Clean up the trash dir the mkdir above created — otherwise it's an
		// orphaned empty ".trash/<id>/" directory forever (this is exactly
		// the leak the 2026-08-18 diagnosis found on disk).
		os.RemoveAll(dstDir) //nolint:errcheck
		return fmt.Errorf("moveToTrash rename: %w", err)
	}
	return nil
}

// renameOrCopy moves src to dst. It first tries the fast atomic os.Rename
// path, which works here because trashDirFor/restoreFile always keep src and
// dst on the same volume. If they still turn out to be on different devices
// (EXDEV) — a defensive fallback for edge cases the volume-root matching
// doesn't cover, e.g. a WatchDirs entry outside every EnumerateScanRoots
// result, or the mount table changing between the two calls — it degrades to
// a streamed copy + fsync + remove-source instead of failing outright.
func (s *TrashService) renameOrCopy(src, dst string) error {
	err := s.osRename(src, dst)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if cerr := copyFileContents(src, dst); cerr != nil {
		os.Remove(dst) //nolint:errcheck  best-effort cleanup of a partial copy
		return fmt.Errorf("cross-device copy: %w", cerr)
	}
	if rerr := os.Remove(src); rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("cross-device remove source: %w", rerr)
	}
	return nil
}

// copyFileContents streams src into dst and fsyncs before returning, so the
// EXDEV fallback in renameOrCopy never removes the source file until the
// copy is durably on disk.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	return out.Close()
}

// RestoreAsset moves an asset back to its original location (including its
// Live Photo companion, if any).
func (s *TrashService) RestoreAsset(id string) error {
	curPath, origPath, liveID, err := s.trashRow(id)
	if err != nil {
		return err
	}
	if err := s.restoreFile(id, curPath, origPath); err != nil {
		return err
	}
	if liveID != "" {
		if lp, lo, _, e := s.trashRow(liveID); e == nil {
			if re := s.restoreFile(liveID, lp, lo); re == nil && s.onCaptionRestore != nil {
				s.onCaptionRestore(liveID) // caption hand-off: restore the Live Photo companion asset's caption too
			}
		}
	}
	if s.onCaptionRestore != nil {
		s.onCaptionRestore(id) // caption hand-off: re-submit after restore, so the caption isn't missing
	}
	return nil
}

// trashRow reads a trash item's current path / original path / Live Photo companion id.
func (s *TrashService) trashRow(id string) (curPath, origPath, liveID string, err error) {
	err = s.db.QueryRow(
		`SELECT file_path, COALESCE(original_path,''), COALESCE(live_photo_video_id,'')
		 FROM assets WHERE id=? AND deleted_at IS NOT NULL`, id,
	).Scan(&curPath, &origPath, &liveID)
	if err == sql.ErrNoRows {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", fmt.Errorf("trashRow: %w", err)
	}
	return curPath, origPath, liveID, nil
}

func (s *TrashService) restoreFile(id, curPath, origPath string) error {
	dst := origPath
	if dst == "" {
		dst = curPath // fallback: no original path, leave it where it is and just clear the flag
	} else {
		if _, err := os.Stat(dst); err == nil {
			dst = dedupePath(dst) // original location taken → rename to dedupe
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("restoreFile mkdir: %w", err)
		}
		// Same-device in the ordinary case (curPath's trash dir was pinned to
		// origPath's own volume by trashDirFor), with the same EXDEV fallback
		// as moveToTrash for the same defensive edge cases.
		if err := s.renameOrCopy(curPath, dst); err != nil {
			return fmt.Errorf("restoreFile rename: %w", err)
		}
	}
	_, err := s.db.Exec(
		`UPDATE assets SET file_path=?, original_path=NULL, deleted_at=NULL WHERE id=?`, dst, id)
	if err != nil {
		return fmt.Errorf("restoreFile update: %w", err)
	}
	return nil
}

// dedupePath inserts " (restored)" before the filename extension, adding a
// number suffix if needed, until the path doesn't already exist.
func dedupePath(p string) string {
	ext := filepath.Ext(p)
	base := p[:len(p)-len(ext)]
	cand := base + " (restored)" + ext
	for i := 2; ; i++ {
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
		cand = fmt.Sprintf("%s (restored %d)%s", base, i, ext)
	}
}

// PurgeAsset permanently deletes a trash item: removes the file + thumbnail
// directory + DB record (including its Live Photo companion, if any).
func (s *TrashService) PurgeAsset(id string) error {
	var curPath, liveID string
	err := s.db.QueryRow(
		`SELECT file_path, COALESCE(live_photo_video_id,'') FROM assets WHERE id=? AND deleted_at IS NOT NULL`, id,
	).Scan(&curPath, &liveID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("PurgeAsset query: %w", err)
	}
	s.purgeFiles(id, curPath)
	if liveID != "" {
		var lp string
		if e := s.db.QueryRow(`SELECT file_path FROM assets WHERE id=?`, liveID).Scan(&lp); e == nil {
			s.purgeFiles(liveID, lp)
			dropClipVector(s.db, liveID) // before the cascade drops asset_clip_idx
			if s.onCaptionDelete != nil {
				s.onCaptionDelete(liveID) // caption hand-off: prevent the agent from retrieving ghost results
			}
			s.db.Exec(`DELETE FROM assets WHERE id=?`, liveID) //nolint:errcheck
		}
	}
	dropClipVector(s.db, id) // before the cascade drops asset_clip_idx
	if s.onCaptionDelete != nil {
		s.onCaptionDelete(id) // caption hand-off: prevent the agent from retrieving ghost results
	}
	if _, err := s.db.Exec(`DELETE FROM assets WHERE id=?`, id); err != nil {
		return fmt.Errorf("PurgeAsset delete: %w", err)
	}
	return nil
}

func (s *TrashService) purgeFiles(id, curPath string) {
	os.Remove(curPath) //nolint:errcheck
	// The per-volume trash directory IS curPath's parent directory (moveToTrash
	// joins filepath.Base(filePath) onto trashDirFor's result to get curPath),
	// so removing it doesn't need to re-derive the volume root — it also
	// correctly cleans up trash items created before this fix, whose curPath
	// still points at the old fixed galleryDir/.trash/<id>/ layout.
	if dir := filepath.Dir(curPath); dir != "." && dir != string(filepath.Separator) {
		os.RemoveAll(dir) //nolint:errcheck
	}
	os.RemoveAll(filepath.Join(s.thumbDir, id)) //nolint:errcheck
}

// ListTrash returns a page of assets currently in the trash (excluding
// live-video companions), ordered by delete time descending. limit/offset
// are expected to already be normalized by the caller (handler applies the
// default + 500 cap); limit<=0 here would return zero rows.
func (s *TrashService) ListTrash(userID string, limit, offset int) ([]Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status, a.deleted_at, COALESCE(a.original_path,'')
FROM assets a
WHERE a.deleted_at IS NOT NULL AND a.is_live_photo_video = 0
ORDER BY a.deleted_at DESC
LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("ListTrash query: %w", err)
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		var a Asset
		var fileSize, durationMs sql.NullInt64
		var takenAt, indexedAt, deletedAt sql.NullTime
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo,
			&indexedAt, &a.Status, &deletedAt, &a.OriginalPath,
		); err != nil {
			return nil, fmt.Errorf("ListTrash scan: %w", err)
		}
		if fileSize.Valid {
			a.FileSize = fileSize.Int64
		}
		if durationMs.Valid {
			a.DurationMs = durationMs.Int64
		}
		if takenAt.Valid {
			t := takenAt.Time
			a.TakenAt = &t
		}
		if indexedAt.Valid {
			t := indexedAt.Time
			a.IndexedAt = &t
		}
		if deletedAt.Valid {
			t := deletedAt.Time
			a.DeletedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EmptyTrash permanently deletes every item in the trash.
func (s *TrashService) EmptyTrash() error {
	ids, err := s.trashIDs("")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.PurgeAsset(id); err != nil {
			return err
		}
	}
	return nil
}

// RestoreAllTrash restores every item in the trash.
func (s *TrashService) RestoreAllTrash() error {
	ids, err := s.trashIDs("")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.RestoreAsset(id); err != nil {
			return err
		}
	}
	return nil
}

// trashIDs returns asset ids currently in the trash (excluding live-video
// companions); whereExtra is an optional additional condition.
func (s *TrashService) trashIDs(whereExtra string) ([]string, error) {
	q := `SELECT id FROM assets WHERE deleted_at IS NOT NULL AND is_live_photo_video = 0`
	if whereExtra != "" {
		q += " AND " + whereExtra
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("trashIDs query: %w", err)
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

// CleanupOrphanTrashDirs sweeps every currently-known trash root (the legacy
// galleryDir fallback plus every currently-mounted scan root) for empty
// ".trash/<id>/" directories and removes them. These are left behind by a
// soft-delete whose file move failed after mkdir already created the
// directory — moveToTrash now cleans up its own failure inline, but this is a
// defensive sweep for anything left over from before this fix, or from a
// crash mid-operation. Only directories that are BOTH empty AND older than
// orphanTrashDirAge are removed, so a moveToTrash still in flight (mkdir done,
// file not yet moved in) is never raced.
func (s *TrashService) CleanupOrphanTrashDirs() {
	roots := map[string]bool{s.galleryDir: true}
	for _, r := range s.scanRoots() {
		roots[r] = true
	}
	for root := range roots {
		s.cleanupOrphanTrashDirsUnder(filepath.Join(root, ".trash"))
	}
}

func (s *TrashService) cleanupOrphanTrashDirsUnder(trashRoot string) {
	entries, err := os.ReadDir(trashRoot)
	if err != nil {
		return // no .trash under this root — nothing to do
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(trashRoot, e.Name())
		inner, err := os.ReadDir(dir)
		if err != nil || len(inner) != 0 {
			continue // not empty (a real trashed file lives here), or unreadable — leave it alone
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < orphanTrashDirAge {
			continue // too fresh — could be a moveToTrash still in flight
		}
		os.Remove(dir) //nolint:errcheck  best-effort; a stray dir is retried on the next sweep
	}
}

// PurgeExpired permanently deletes trash items deleted before (now - retentionDays).
func (s *TrashService) PurgeExpired(retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	ids, err := s.trashIDs(fmt.Sprintf("deleted_at < datetime('now', '-%d days')", retentionDays))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.PurgeAsset(id); err != nil {
			return err
		}
	}
	return nil
}
