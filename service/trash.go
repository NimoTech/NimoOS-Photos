package service

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// TrashService 实现软删除（回收站）：删除把原文件移到 <galleryDir>/.trash/<id>/，
// DB 记 deleted_at；恢复移回；永久删除 rm 文件 + 缩略图 + DB 记录。
type TrashService struct {
	db         *sql.DB
	galleryDir string // 回收站根目录所在的 gallery 根（.trash 放它下面）
	thumbDir   string // 缩略图根目录，用于永久删除时清理
}

// NewTrashService 构造 TrashService。
func NewTrashService(db *sql.DB, galleryDir, thumbDir string) *TrashService {
	return &TrashService{db: db, galleryDir: galleryDir, thumbDir: thumbDir}
}

func (s *TrashService) trashDir(id string) string {
	return filepath.Join(s.galleryDir, ".trash", id)
}

// TrashAsset 把一个资产移入回收站（含其 Live Photo 视频伴随项）。
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
			_ = s.moveToTrash(liveID, livePath)
		}
	}
	return nil
}

func (s *TrashService) moveToTrash(id, filePath string) error {
	dstDir := s.trashDir(id)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("moveToTrash mkdir: %w", err)
	}
	dst := filepath.Join(dstDir, filepath.Base(filePath))
	if err := os.Rename(filePath, dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("moveToTrash rename: %w", err)
	}
	_, err := s.db.Exec(
		`UPDATE assets SET original_path=?, file_path=?, deleted_at=CURRENT_TIMESTAMP WHERE id=?`,
		filePath, dst, id)
	if err != nil {
		return fmt.Errorf("moveToTrash update: %w", err)
	}
	return nil
}

// RestoreAsset 把一个资产移回原位（含 Live Photo 伴随项）。
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
			_ = s.restoreFile(liveID, lp, lo)
		}
	}
	return nil
}

// trashRow 读取一个回收站项的当前路径/原路径/live 伴随 id。
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
		dst = curPath // 兜底：没有原路径就留在原地，仅清标记
	} else {
		if _, err := os.Stat(dst); err == nil {
			dst = dedupePath(dst) // 原位被占用 → 去重命名
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

// dedupePath 在文件名扩展名前插入 " (restored)"，必要时加序号，直到路径不存在。
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

// PurgeAsset 永久删除一个回收站项：rm 文件 + 缩略图目录 + DB 记录（含 Live 伴随项）。
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
			s.db.Exec(`DELETE FROM assets WHERE id=?`, liveID) //nolint:errcheck
		}
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

// ListTrash 返回所有在回收站的资产（非 live-video），按删除时间倒序。
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

// EmptyTrash 永久删除所有回收站项。
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

// RestoreAllTrash 恢复所有回收站项。
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

// trashIDs 返回回收站中（非 live-video）资产 id；whereExtra 为附加条件（可空）。
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

// PurgeExpired 永久删除删除时间早于 (now - retentionDays) 的回收站项。
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
