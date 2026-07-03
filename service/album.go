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
	// Cover resolution uses two layers:
	// 1. Membership guard: only honour cover_asset_id when the asset is still a
	//    member of the album (guards against dangling pointers after removal).
	// 2. Stable implicit fallback: when no valid explicit cover exists, fall back
	//    to the first item by position (then rowid as tiebreaker), so adding new
	//    photos never changes an implicit cover.
	rows, err := s.db.Query(`
		SELECT a.id, a.name, a.created_at,
		       COALESCE(
		           (SELECT aa3.asset_id FROM album_assets aa3
		               WHERE aa3.album_id = a.id AND aa3.asset_id = NULLIF(a.cover_asset_id,'')),
		           (SELECT aa2.asset_id FROM album_assets aa2
		               WHERE aa2.album_id = a.id
		               ORDER BY aa2.position ASC, aa2.rowid ASC LIMIT 1),
		           '') AS cover,
		       COUNT(aa.asset_id) AS cnt,
		       MIN(sp.taken_at) AS date_start,
		       MAX(sp.taken_at) AS date_end,
		       SUM(CASE WHEN sp.is_live_photo_video = 0 AND sp.deleted_at IS NULL AND sp.offline = 0
		                 AND sp.mime_type NOT LIKE 'video/%' THEN 1 ELSE 0 END) AS photo_cnt,
		       SUM(CASE WHEN sp.is_live_photo_video = 0 AND sp.deleted_at IS NULL AND sp.offline = 0
		                 AND sp.mime_type LIKE 'video/%' THEN 1 ELSE 0 END) AS video_cnt
		FROM albums a
		LEFT JOIN album_assets aa ON aa.album_id = a.id
		LEFT JOIN assets sp ON sp.id = aa.asset_id
		GROUP BY a.id ORDER BY a.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var albums []Album
	for rows.Next() {
		var a Album
		var dateStart, dateEnd sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt, &a.CoverAssetID, &a.AssetCount,
			&dateStart, &dateEnd, &a.PhotoCount, &a.VideoCount); err != nil {
			return nil, err
		}
		a.DateStart = dateStart.String
		a.DateEnd = dateEnd.String
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

func (s *AlbumService) Get(id string) (*Album, error) {
	var a Album
	var dateStart, dateEnd sql.NullString
	// Cover resolution (same two-layer logic as List):
	// - Membership guard: only honour cover_asset_id when it is still a member.
	// - Stable implicit fallback: first item by position/rowid, so adding photos
	//   never changes an implicit cover.
	err := s.db.QueryRow(`
		SELECT id, name, created_at,
		       COALESCE(
		           (SELECT aa3.asset_id FROM album_assets aa3
		               WHERE aa3.album_id = albums.id AND aa3.asset_id = NULLIF(albums.cover_asset_id,'')),
		           (SELECT aa2.asset_id FROM album_assets aa2
		               WHERE aa2.album_id = albums.id
		               ORDER BY aa2.position ASC, aa2.rowid ASC LIMIT 1),
		           ''),
		       (SELECT MIN(s.taken_at) FROM album_assets aa
		          JOIN assets s ON s.id = aa.asset_id WHERE aa.album_id = albums.id),
		       (SELECT MAX(s.taken_at) FROM album_assets aa
		          JOIN assets s ON s.id = aa.asset_id WHERE aa.album_id = albums.id)
		FROM albums WHERE id=?`, id,
	).Scan(&a.ID, &a.Name, &a.CreatedAt, &a.CoverAssetID, &dateStart, &dateEnd)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	a.DateStart = dateStart.String
	a.DateEnd = dateEnd.String
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

func (s *AlbumService) UpdateName(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrInvalidInput
	}

	var existingID string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE id=?`, id).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	var conflictID string
	err = s.db.QueryRow(`SELECT id FROM albums WHERE name=? AND id<>? LIMIT 1`, name, id).Scan(&conflictID)
	if err == nil {
		return ErrAlbumNameExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	_, err = s.db.Exec(`UPDATE albums SET name=? WHERE id=?`, name, id)
	return err
}

func (s *AlbumService) UpdateCover(id, assetID string) error {
	var albumID string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE id=?`, id).Scan(&albumID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	var dummy string
	err = s.db.QueryRow(
		`SELECT asset_id FROM album_assets WHERE album_id=? AND asset_id=?`,
		id, assetID,
	).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCoverNotInAlbum
	}
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`UPDATE albums SET cover_asset_id=? WHERE id=?`, assetID, id)
	return err
}

func (s *AlbumService) AddAsset(albumID, assetID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var nextPos int
	err = tx.QueryRow(
		`SELECT COALESCE(MAX(position), -1) + 1 FROM album_assets WHERE album_id=?`,
		albumID,
	).Scan(&nextPos)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO album_assets(album_id, asset_id, added_at, position) VALUES(?,?,?,?)`,
		albumID, assetID, time.Now(), nextPos,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AlbumService) RemoveAsset(albumID, assetID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM album_assets WHERE album_id=? AND asset_id=?`, albumID, assetID); err != nil {
		return err
	}
	// Write-side hygiene: clear cover_asset_id when it points at the removed asset
	// so the DB doesn't keep a dangling pointer. The read-side membership guard in
	// List/Get handles any remaining dangling covers (e.g. from external deletions).
	if _, err := tx.Exec(`UPDATE albums SET cover_asset_id=NULL WHERE id=? AND cover_asset_id=?`, albumID, assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// BatchAddAssets adds multiple assets to an album in a single transaction.
// Idempotent (INSERT OR IGNORE). Returns ErrNotFound if the album does not exist.
func (s *AlbumService) BatchAddAssets(albumID string, assetIDs []string) error {
	var dummy string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE id=?`, albumID).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
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

	var nextPos int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(position), -1) + 1 FROM album_assets WHERE album_id=?`,
		albumID,
	).Scan(&nextPos); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO album_assets(album_id, asset_id, added_at, position) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	for _, id := range assetIDs {
		res, err := stmt.Exec(albumID, id, now, nextPos)
		if err != nil {
			return err
		}
		// Only advance position when a new row was actually inserted (INSERT OR IGNORE returns RowsAffected=0 for duplicates)
		if n, _ := res.RowsAffected(); n > 0 {
			nextPos++
		}
	}
	return tx.Commit()
}

// ReorderAssets replaces position 0..n-1 for every asset in the album.
// assetIDs must be a strict permutation of the album's current asset set:
// empty slice, duplicates, extra IDs, or missing IDs all return ErrInvalidInput.
func (s *AlbumService) ReorderAssets(albumID string, assetIDs []string) error {
	// Reject empty input early.
	if len(assetIDs) == 0 {
		return ErrInvalidInput
	}

	// Reject duplicate IDs (a strict-set requirement).
	seen := make(map[string]struct{}, len(assetIDs))
	for _, id := range assetIDs {
		if _, dup := seen[id]; dup {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}

	// Verify album exists.
	var dummy string
	err := s.db.QueryRow(`SELECT id FROM albums WHERE id=?`, albumID).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	// Load current asset set.
	rows, err := s.db.Query(`SELECT asset_id FROM album_assets WHERE album_id=?`, albumID)
	if err != nil {
		return err
	}
	current := map[string]struct{}{}
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			return err
		}
		current[aid] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Strict set equality: any difference is an invalid input.
	if len(current) != len(assetIDs) {
		return ErrInvalidInput
	}
	for _, id := range assetIDs {
		if _, ok := current[id]; !ok {
			return ErrInvalidInput
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`UPDATE album_assets SET position=? WHERE album_id=? AND asset_id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i, id := range assetIDs {
		if _, err := stmt.Exec(i, albumID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *AlbumService) ListAssets(albumID string) ([]Asset, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
		       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
		       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
		       a.indexed_at, a.status
		FROM assets a
		JOIN album_assets aa ON aa.asset_id = a.id
		WHERE aa.album_id = ? AND a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
		ORDER BY aa.position ASC, aa.added_at ASC
	`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssets(rows)
}
