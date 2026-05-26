package service

import (
	"database/sql"
	"fmt"
	"time"
)

// FavoritesService manages per-user favorite records on assets.
type FavoritesService struct {
	db *sql.DB
}

func NewFavoritesService(db *sql.DB) *FavoritesService {
	return &FavoritesService{db: db}
}

// ListFavoritesOpts controls pagination for List.
type ListFavoritesOpts struct {
	Limit  int
	Offset int
}

func (s *FavoritesService) Favorite(userID, assetID string) (time.Time, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM assets WHERE id=?`, assetID).Scan(&exists)
	if err == sql.ErrNoRows {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("Favorite check asset: %w", err)
	}

	now := time.Now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO asset_favorites(user_id, asset_id, favorited_at) VALUES(?, ?, ?)
		 ON CONFLICT(user_id, asset_id) DO NOTHING`,
		userID, assetID, now,
	); err != nil {
		return time.Time{}, fmt.Errorf("Favorite insert: %w", err)
	}

	var favAt time.Time
	if err := s.db.QueryRow(
		`SELECT favorited_at FROM asset_favorites WHERE user_id=? AND asset_id=?`,
		userID, assetID,
	).Scan(&favAt); err != nil {
		return time.Time{}, fmt.Errorf("Favorite read back: %w", err)
	}
	return favAt, nil
}

func (s *FavoritesService) Unfavorite(userID, assetID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM asset_favorites WHERE user_id=? AND asset_id=?`,
		userID, assetID,
	); err != nil {
		return fmt.Errorf("Unfavorite: %w", err)
	}
	return nil
}

func (s *FavoritesService) ListIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT asset_id FROM asset_favorites WHERE user_id=? ORDER BY favorited_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("ListIDs: %w", err)
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

func (s *FavoritesService) List(userID string, opts ListFavoritesOpts) ([]Asset, error) {
	q := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM asset_favorites f
JOIN assets a ON a.id = f.asset_id
LEFT JOIN asset_exif e ON e.asset_id = a.id
WHERE f.user_id = ?
ORDER BY f.favorited_at DESC`

	args := []interface{}{userID}
	if opts.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("List query: %w", err)
	}
	defer rows.Close()
	return scanAssetsDetailedWithFav(rows)
}

func (s *FavoritesService) IsFavorited(userID, assetID string) (bool, error) {
	var v int
	err := s.db.QueryRow(
		`SELECT 1 FROM asset_favorites WHERE user_id=? AND asset_id=?`,
		userID, assetID,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("IsFavorited: %w", err)
	}
	return true, nil
}
