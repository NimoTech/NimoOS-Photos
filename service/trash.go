package service

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// TrashService implements soft delete (trash): deleting moves the original
// file to <galleryDir>/.trash/<id>/ and records deleted_at in the DB;
// restoring moves it back; a permanent delete removes the file + thumbnail +
// DB record.
type TrashService struct {
	db         *sql.DB
	galleryDir string // gallery root that holds the trash root (.trash lives under it)
	thumbDir   string // thumbnail root, cleaned up on permanent delete

	// onCaptionDelete/onCaptionRestore are the caption hand-off hooks (Task
	// 4). Function fields avoid TrashService importing CaptionFeeder
	// directly (same injection convention as MountGuard/Embedder). Safely
	// skipped when nil (not wired up / tests).
	onCaptionDelete  func(assetID string)
	onCaptionRestore func(assetID string)
}

// NewTrashService constructs a TrashService.
func NewTrashService(db *sql.DB, galleryDir, thumbDir string) *TrashService {
	return &TrashService{db: db, galleryDir: galleryDir, thumbDir: thumbDir}
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

func (s *TrashService) trashDir(id string) string {
	return filepath.Join(s.galleryDir, ".trash", id)
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
	dstDir := s.trashDir(id)
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
		return fmt.Errorf("moveToTrash update: %w", err)
	}
	if err := os.Rename(filePath, dst); err != nil && !os.IsNotExist(err) {
		// Roll back so the row keeps pointing at the still-present original file.
		s.db.Exec(`UPDATE assets SET original_path=NULL, file_path=?, deleted_at=NULL WHERE id=?`, filePath, id) //nolint:errcheck
		return fmt.Errorf("moveToTrash rename: %w", err)
	}
	return nil
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
		if err := os.Rename(curPath, dst); err != nil && !os.IsNotExist(err) {
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
	os.Remove(curPath)                          //nolint:errcheck
	os.RemoveAll(s.trashDir(id))                //nolint:errcheck
	os.RemoveAll(filepath.Join(s.thumbDir, id)) //nolint:errcheck
}

// ListTrash returns every asset currently in the trash (excluding
// live-video companions), ordered by delete time descending.
func (s *TrashService) ListTrash(userID string) ([]Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status, a.deleted_at, COALESCE(a.original_path,'')
FROM assets a
WHERE a.deleted_at IS NOT NULL AND a.is_live_photo_video = 0
ORDER BY a.deleted_at DESC`)
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
