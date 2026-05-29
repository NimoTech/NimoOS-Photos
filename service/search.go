package service

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// textEmbedder is the minimal interface SearchService needs from the ML layer.
// It is satisfied by MLProvider (defined in indexer.go) without coupling.
type textEmbedder interface {
	CLIPTextEmbed(text string) ([]float32, error)
}

// SearchFilters contains optional time-based filters for SmartSearch.
type SearchFilters struct {
	Year  int
	Month int
}

// SearchService provides semantic search, timeline, person, and asset CRUD.
type SearchService struct {
	db *sql.DB
	ml textEmbedder
}

// NewSearchService constructs a SearchService.
// ml may be nil when only non-CLIP methods (Timeline, ListAssets, …) are used.
func NewSearchService(db *sql.DB, ml textEmbedder) *SearchService {
	return &SearchService{db: db, ml: ml}
}

// ─── Smart (CLIP) search ─────────────────────────────────────────────────────

// minMatchSimilarity, when > 0, drops SmartSearch results whose cosine similarity
// (1 - d²/2 over unit-length CLIP vectors, i.e. Asset.MatchScore) is below it.
//
// Why this knob exists: KNN always returns the k nearest neighbours regardless of
// absolute similarity, so on a small library every query returns a long tail of
// barely-related fillers. Empirically for this corpus the distribution is:
// noise ~0.16–0.19, mediocre ~0.20–0.23, confident matches ≥0.24. Setting this to
// ~0.22 cuts the irrelevant tail (and the "irrelevant videos at high %" symptom).
//
// Left at 0 (no filtering) by default — current behaviour is unchanged. Bump it
// here to enable a relevance floor.
const minMatchSimilarity = 0.0

// SmartSearch performs KNN vector search on CLIP embeddings filtered by optional
// year/month, returning at most limit results.
func (s *SearchService) SmartSearch(query string, limit int, filters SearchFilters) ([]Asset, error) {
	queryVec, err := s.ml.CLIPTextEmbed(query)
	if err != nil {
		return nil, fmt.Errorf("SmartSearch embed: %w", err)
	}
	blob := sqlite.SerializeFloat32(queryVec)

	// Base KNN query — sqlite-vec uses "MATCH ? AND k = ?" syntax.
	baseSQL := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status, vec.distance
FROM clip_embeddings AS vec
JOIN asset_clip_idx AS idx ON idx.rowid = vec.rowid
JOIN assets a ON a.id = idx.asset_id
WHERE vec.embedding MATCH ? AND k = ?
  AND a.is_live_photo_video = 0
  AND a.deleted_at IS NULL`

	args := []any{blob, limit}

	var clauses []string
	if filters.Year > 0 {
		clauses = append(clauses, `strftime('%Y', a.taken_at) = ?`)
		args = append(args, fmt.Sprintf("%04d", filters.Year))
	}
	if filters.Month > 0 {
		clauses = append(clauses, `strftime('%m', a.taken_at) = ?`)
		args = append(args, fmt.Sprintf("%02d", filters.Month))
	}

	q := baseSQL
	if len(clauses) > 0 {
		q += "\n  AND " + strings.Join(clauses, "\n  AND ")
	}
	q += "\nORDER BY vec.distance"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("SmartSearch query: %w", err)
	}
	defer rows.Close()

	assets, err := scanSearchAssets(rows)
	if err != nil {
		return nil, err
	}
	if minMatchSimilarity > 0 {
		kept := assets[:0]
		for _, a := range assets {
			if a.MatchScore == nil || *a.MatchScore >= minMatchSimilarity {
				kept = append(kept, a)
			}
		}
		assets = kept
	}
	// Attach named persons so the client can offer a People filter on results.
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	return assets, nil
}

// attachNamedFaces fills each asset's Faces with the names of the named persons
// detected in it (deduplicated). Unnamed/hidden persons and excluded faces are
// skipped. A no-op for an empty slice. Mirrors FavoritesService.attachNamedFaces.
func (s *SearchService) attachNamedFaces(assets []Asset) error {
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

// ─── Timeline ────────────────────────────────────────────────────────────────

// Timeline returns all non-live-photo-video assets grouped by year/month in
// descending chronological order. Assets without taken_at go into year=0, month=0.
// The LEFT JOIN on asset_exif lets the frontend aggregate filter facets
// (camera, location) without an extra fetch per asset.
func (s *SearchService) Timeline(userID string) ([]TimelineGroup, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("Timeline query: %w", err)
	}
	defer rows.Close()

	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}

	type key struct{ year, month int }
	var order []key
	groups := map[key]*TimelineGroup{}

	for _, a := range assets {
		var k key
		if a.TakenAt != nil {
			k = key{a.TakenAt.Year(), int(a.TakenAt.Month())}
		} else if a.IndexedAt != nil {
			k = key{a.IndexedAt.Year(), int(a.IndexedAt.Month())}
		}

		if _, exists := groups[k]; !exists {
			order = append(order, k)
			groups[k] = &TimelineGroup{Year: k.year, Month: k.month}
		}
		groups[k].Assets = append(groups[k].Assets, a)
	}

	result := make([]TimelineGroup, 0, len(order))
	for _, k := range order {
		result = append(result, *groups[k])
	}
	return result, nil
}

// ─── Person search ───────────────────────────────────────────────────────────

// PersonAssets returns paginated assets containing a given person's face.
func (s *SearchService) PersonAssets(personID string, limit, offset int) ([]Asset, error) {
	rows, err := s.db.Query(`
SELECT DISTINCT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status
FROM assets a
JOIN face_detections fd ON fd.asset_id = a.id
JOIN face_person fp ON fp.face_id = fd.id
WHERE fp.person_id = ? AND a.is_live_photo_video = 0 AND a.deleted_at IS NULL
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
LIMIT ? OFFSET ?`, personID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("PersonAssets query: %w", err)
	}
	defer rows.Close()
	return scanAssets(rows)
}

// ─── Person management ───────────────────────────────────────────────────────

// ListPersons returns all persons in insertion order.
func (s *SearchService) ListPersons() ([]Person, error) {
	rows, err := s.db.Query(`SELECT p.id, p.name, COALESCE(p.cover_asset_id,'') FROM persons p ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("ListPersons query: %w", err)
	}
	defer rows.Close()

	var persons []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.CoverAssetID); err != nil {
			return nil, err
		}
		persons = append(persons, p)
	}
	return persons, rows.Err()
}

// UpdatePersonName sets the display name for a person; returns ErrNotFound when
// the person does not exist.
func (s *SearchService) UpdatePersonName(id, name string) error {
	res, err := s.db.Exec(`UPDATE persons SET name=? WHERE id=?`, name, id)
	if err != nil {
		return fmt.Errorf("UpdatePersonName: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MergePersons reassigns all face_person rows from fromID to intoID, then
// deletes the source person and recomputes the centroid/confidence/cover of
// intoID — all within a single transaction.
func (s *SearchService) MergePersons(fromID, intoID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("MergePersons begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.Exec(`UPDATE face_person SET person_id=? WHERE person_id=?`, intoID, fromID); err != nil {
		return fmt.Errorf("MergePersons update: %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM persons WHERE id=?`, fromID); err != nil {
		return fmt.Errorf("MergePersons delete: %w", err)
	}
	if err = recomputeOneCentroidTx(tx, intoID); err != nil {
		return fmt.Errorf("MergePersons recompute: %w", err)
	}
	return tx.Commit()
}

// ─── Asset CRUD ──────────────────────────────────────────────────────────────

// ListAssets returns paginated assets (non-live-photo-video) ordered by
// descending taken_at / indexed_at.
func (s *SearchService) ListAssets(userID string, limit, offset int) ([]Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("ListAssets query: %w", err)
	}
	defer rows.Close()
	return scanAssetsDetailedWithFav(rows)
}

// GetAsset returns a single asset by ID; returns ErrNotFound when absent.
func (s *SearchService) GetAsset(userID, id string) (*Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.id = ?`, userID, id)
	if err != nil {
		return nil, fmt.Errorf("GetAsset query: %w", err)
	}
	defer rows.Close()

	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, ErrNotFound
	}
	return &assets[0], nil
}

// DeleteAsset removes an asset by ID; returns ErrNotFound when absent.
func (s *SearchService) DeleteAsset(id string) error {
	res, err := s.db.Exec(`DELETE FROM assets WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("DeleteAsset: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
