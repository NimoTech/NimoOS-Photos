package service

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AlbumService struct {
	db *sql.DB
}

func NewAlbumService(db *sql.DB) *AlbumService {
	return &AlbumService{db: db}
}

func (s *AlbumService) Create(name string) (*Album, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidInput
	}

	var existing string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE name=? LIMIT 1`, name).Scan(&existing)
	if err == nil {
		return nil, ErrAlbumNameExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	a := &Album{
		ID:        uuid.NewString(),
		Name:      name,
		CreatedAt: time.Now(),
	}
	_, err = s.db.Exec(
		`INSERT INTO albums(id, name, created_at) VALUES(?,?,?)`,
		a.ID, a.Name, a.CreatedAt,
	)
	return a, err
}

func (s *AlbumService) List() ([]Album, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.name, a.created_at, COALESCE(a.cover_asset_id,''),
		       COUNT(aa.asset_id) AS cnt
		FROM albums a
		LEFT JOIN album_assets aa ON aa.album_id = a.id
		GROUP BY a.id ORDER BY a.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var albums []Album
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt, &a.CoverAssetID, &a.AssetCount); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

func (s *AlbumService) Get(id string) (*Album, error) {
	var a Album
	err := s.db.QueryRow(`
		SELECT id, name, created_at, COALESCE(cover_asset_id,'') FROM albums WHERE id=?`, id,
	).Scan(&a.ID, &a.Name, &a.CreatedAt, &a.CoverAssetID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &a, err
}

func (s *AlbumService) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM albums WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *AlbumService) AddAsset(albumID, assetID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO album_assets(album_id, asset_id, added_at) VALUES(?,?,?)`,
		albumID, assetID, time.Now(),
	)
	return err
}

func (s *AlbumService) RemoveAsset(albumID, assetID string) error {
	_, err := s.db.Exec(`DELETE FROM album_assets WHERE album_id=? AND asset_id=?`, albumID, assetID)
	return err
}

// BatchAddAssets 在单 transaction 内一次添加多个 asset。
// 幂等（INSERT OR IGNORE）。album 不存在返回 ErrNotFound。
func (s *AlbumService) BatchAddAssets(albumID string, assetIDs []string) error {
	var dummy string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE id=?`, albumID).Scan(&dummy)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO album_assets(album_id, asset_id, added_at) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	for _, id := range assetIDs {
		if _, err := stmt.Exec(albumID, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *AlbumService) ListAssets(albumID string) ([]Asset, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
		       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
		       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
		       a.indexed_at, a.status
		FROM assets a
		JOIN album_assets aa ON aa.asset_id = a.id
		WHERE aa.album_id = ? AND a.is_live_photo_video = 0 AND a.deleted_at IS NULL
		ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
	`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssets(rows)
}
