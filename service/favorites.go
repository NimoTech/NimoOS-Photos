package service

import (
	"database/sql"
	"fmt"
	"strings"
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM asset_favorites f
JOIN assets a ON a.id = f.asset_id
LEFT JOIN asset_exif e ON e.asset_id = a.id
WHERE f.user_id = ? AND a.deleted_at IS NULL AND a.offline = 0
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
	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)
	return assets, nil
}

// attachNamedFaces fills each asset's Faces with the names of the named
// persons detected in it (deduplicated). Unnamed/hidden persons and excluded
// faces are skipped. A no-op for an empty slice.
func (s *FavoritesService) attachNamedFaces(assets []Asset) error {
	if len(assets) == 0 {
		return nil
	}
	idIndex := make(map[string]int, len(assets))
	placeholders := make([]string, len(assets))
	args := make([]interface{}, len(assets))
	for i, a := range assets {
		idIndex[a.ID] = i
		placeholders[i] = "?"
		args[i] = a.ID
	}
	rows, err := s.db.Query(`
SELECT fd.asset_id, p.name
FROM face_detections fd
JOIN face_person fp ON fp.face_id = fd.id
JOIN persons p ON p.id = fp.person_id
WHERE fd.asset_id IN (`+strings.Join(placeholders, ",")+`)
  AND p.name <> '' AND COALESCE(p.hidden, 0) = 0 AND COALESCE(fd.excluded, 0) = 0`, args...)
	if err != nil {
		return fmt.Errorf("attachNamedFaces query: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]map[string]bool, len(assets))
	for rows.Next() {
		var assetID, name string
		if err := rows.Scan(&assetID, &name); err != nil {
			return err
		}
		i, ok := idIndex[assetID]
		if !ok {
			continue
		}
		if seen[assetID] == nil {
			seen[assetID] = map[string]bool{}
		}
		if seen[assetID][name] {
			continue
		}
		seen[assetID][name] = true
		assets[i].Faces = append(assets[i].Faces, name)
	}
	return rows.Err()
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

// Top returns up to limit favorited assets for userID, ordered by view count
// (desc) with favorited_at (desc) as tie-breaker / fallback for never-viewed
// favorites. The SELECT column list MUST match scanAssetsDetailedWithFav's
// expectations (detailed columns + trailing favorited_at).
func (s *FavoritesService) Top(userID string, limit int) ([]Asset, error) {
	if limit <= 0 {
		limit = 5
	}
	q := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM asset_favorites f
JOIN assets a ON a.id = f.asset_id
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_views v ON v.user_id = f.user_id AND v.asset_id = f.asset_id
WHERE f.user_id = ? AND a.deleted_at IS NULL AND a.offline = 0
ORDER BY COALESCE(v.view_count, 0) DESC, f.favorited_at DESC
LIMIT ?`

	rows, err := s.db.Query(q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("Top query: %w", err)
	}
	defer rows.Close()
	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)
	return assets, nil
}
