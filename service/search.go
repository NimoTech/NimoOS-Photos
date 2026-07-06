package service

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// AssetFilter carries optional geo filters for ListAssets.
// PlaceKey > 0 filters by city (geonameid).
// SpotKey non-empty filters by spot grid cell in format "cityID:gx:gy".
// When both are set, SpotKey takes precedence (it already implies the city).
type AssetFilter struct {
	PlaceKey int32
	SpotKey  string
	// AssetIDs, when non-nil, restricts results to exactly these IDs. The spot
	// filter resolves SpotKey to a cluster's member IDs upstream and passes them
	// here so the library count matches the spot dialog count exactly. A non-nil
	// but empty slice yields no results (the spot resolved to zero photos).
	AssetIDs []string
}

// ParseSpotKey splits a spot_key of the form "cityID:gx:gy" into its three
// integer components. Returns an error if the format is invalid.
func ParseSpotKey(key string) (cityID int32, gx, gy int, err error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid spot_key %q: expected cityID:gx:gy", key)
	}
	c, e := strconv.ParseInt(parts[0], 10, 32)
	if e != nil {
		return 0, 0, 0, fmt.Errorf("invalid spot_key city: %w", e)
	}
	gxi, e := strconv.Atoi(parts[1])
	if e != nil {
		return 0, 0, 0, fmt.Errorf("invalid spot_key gx: %w", e)
	}
	gyi, e := strconv.Atoi(parts[2])
	if e != nil {
		return 0, 0, 0, fmt.Errorf("invalid spot_key gy: %w", e)
	}
	return int32(c), gxi, gyi, nil
}

// textEmbedder is the minimal interface SearchService needs from the ML layer.
// It is satisfied by MLProvider (defined in indexer.go) without coupling.
type textEmbedder interface {
	CLIPTextEmbed(text string) ([]float32, error)
}

// SearchFilters contains optional time-based filters for SmartSearch.
type SearchFilters struct {
	Year  int
	Month int
	// IncludeOCR additionally matches the query as a case-insensitive substring
	// of each asset's recognized text (asset_ocr) and pins those exact hits to
	// the top with MatchScore 1.0. Deliberately NOT client-controllable
	// (json:"-"): the search route opts in server-side, while Smart View
	// semantic conditions — which also call SmartSearch — must stay pure-CLIP,
	// or photos containing the literal condition text would pollute the
	// threshold semantics with 1.0 scores.
	IncludeOCR bool `json:"-"`
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
// is below it. It is compared against Asset.MatchScore, which since scan.go's
// displayScore is the *recalibrated* [0,1] display score — not the raw CLIP
// cosine — so this threshold's semantics are display-scale, not raw-cosine-scale.
//
// Why this knob exists: KNN always returns the k nearest neighbours regardless of
// absolute similarity, so on a small library every query returns a long tail of
// barely-related fillers.
//
// The empirical band below (noise ~0.16–0.19, mediocre ~0.20–0.23, confident
// ≥0.24) was measured against openai CLIP's raw cosine and is now STALE: the
// model has since moved to nllb-clip-large-siglip__v1, whose raw cosine
// distribution sits an order of magnitude lower (see simDisplayFloor/Ceil in
// scan.go) and, post-recalibration, no longer lines up with these numbers.
// A fresh empirical pass against the recalibrated MatchScore is needed after a
// full library re-embed before reusing this knob.
//
// Left at 0 (no filtering) by default — current behaviour (and value) is
// unchanged by this recalibration. Bump it here to enable a relevance floor.
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
	// asset_exif is LEFT JOINed for latitude/longitude so the client can offer a
	// Places filter on results (reverse-geocoded to country in the UI).
	baseSQL := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status, e.latitude, e.longitude, vec.distance
FROM clip_embeddings AS vec
JOIN asset_clip_idx AS idx ON idx.rowid = vec.rowid
JOIN assets a ON a.id = idx.asset_id
LEFT JOIN asset_exif AS e ON e.asset_id = a.id
WHERE vec.embedding MATCH ? AND k = ?
  AND a.is_live_photo_video = 0
  AND a.deleted_at IS NULL AND a.offline = 0`

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

	// Exact-text OCR hits outrank semantic guesses: prepend them at score 1.0
	// and drop the CLIP duplicate when the same asset matched both ways.
	if filters.IncludeOCR {
		ocrHits, err := s.ocrSearch(query, limit, filters)
		if err != nil {
			return nil, err
		}
		assets = mergeOCRFirst(ocrHits, assets, limit)
	}

	// Attach named persons so the client can offer a People filter on results.
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)
	return assets, nil
}

// ocrSearch returns assets whose recognized text (asset_ocr) contains query as
// a case-insensitive substring, newest first. The constant 0.0 distance makes
// scanSearchAssets derive MatchScore 1.0, marking these as exact hits.
func (s *SearchService) ocrSearch(query string, limit int, filters SearchFilters) ([]Asset, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	q := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status, e.latitude, e.longitude, 0.0 AS distance
FROM asset_ocr o
JOIN assets a ON a.id = o.asset_id
LEFT JOIN asset_exif AS e ON e.asset_id = a.id
WHERE instr(lower(o.text), lower(?)) > 0
  AND a.is_live_photo_video = 0
  AND a.deleted_at IS NULL AND a.offline = 0`

	args := []any{query}
	if filters.Year > 0 {
		q += "\n  AND strftime('%Y', a.taken_at) = ?"
		args = append(args, fmt.Sprintf("%04d", filters.Year))
	}
	if filters.Month > 0 {
		q += "\n  AND strftime('%m', a.taken_at) = ?"
		args = append(args, fmt.Sprintf("%02d", filters.Month))
	}
	q += "\nORDER BY COALESCE(a.taken_at, a.indexed_at) DESC\nLIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ocrSearch query: %w", err)
	}
	defer rows.Close()
	assets, err := scanSearchAssets(rows)
	if err != nil {
		return nil, err
	}
	for i := range assets {
		assets[i].MatchedBy = "ocr"
	}
	return assets, nil
}

// mergeOCRFirst concatenates ocr hits before clip results, dropping duplicate
// asset IDs (the OCR entry wins) and trimming to limit.
func mergeOCRFirst(ocr, clip []Asset, limit int) []Asset {
	seen := make(map[string]struct{}, len(ocr)+len(clip))
	out := make([]Asset, 0, len(ocr)+len(clip))
	for _, a := range append(ocr, clip...) {
		if _, dup := seen[a.ID]; dup {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("Timeline query: %w", err)
	}
	defer rows.Close()

	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)

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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status
FROM assets a
JOIN face_detections fd ON fd.asset_id = a.id
JOIN face_person fp ON fp.face_id = fd.id
WHERE fp.person_id = ? AND a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
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
//
// An optional AssetFilter may be passed as the fourth argument to restrict
// results by city (PlaceKey) or spot grid cell (SpotKey). When SpotKey is
// set it takes precedence over PlaceKey.
func (s *SearchService) ListAssets(userID string, limit, offset int, filters ...AssetFilter) ([]Asset, error) {
	var f AssetFilter
	if len(filters) > 0 {
		f = filters[0]
	}

	// An explicit ID set (spot filter) short-circuits the geo path: restrict to
	// exactly these IDs. A non-nil empty slice means "no photos" → return early.
	if f.AssetIDs != nil {
		if len(f.AssetIDs) == 0 {
			return []Asset{}, nil
		}
		return s.listAssetsByIDs(userID, f.AssetIDs)
	}

	// Build the geo JOIN + WHERE clauses.
	var geoJoin string
	var geoClauses []string
	var geoArgs []any

	if f.SpotKey != "" {
		cityID, gx, gy, err := ParseSpotKey(f.SpotKey)
		if err != nil {
			return nil, fmt.Errorf("ListAssets: %w", err)
		}
		geoJoin = `JOIN asset_geo g ON g.asset_id = a.id`
		geoClauses = append(geoClauses,
			`g.city_id = ?`,
			`CAST(g.lat / 0.01 AS INT) = ?`,
			`CAST(g.lon / 0.01 AS INT) = ?`,
		)
		geoArgs = append(geoArgs, cityID, gx, gy)
	} else if f.PlaceKey > 0 {
		geoJoin = `JOIN asset_geo g ON g.asset_id = a.id`
		geoClauses = append(geoClauses, `g.city_id = ?`)
		geoArgs = append(geoArgs, f.PlaceKey)
	}

	whereExtra := ""
	if len(geoClauses) > 0 {
		whereExtra = " AND " + strings.Join(geoClauses, " AND ")
	}

	// userID must be first (for the favorites LEFT JOIN ON clause).
	args := []any{userID}
	args = append(args, geoArgs...)
	args = append(args, limit, offset)

	q := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
` + geoJoin + `
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0` + whereExtra + `
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
LIMIT ? OFFSET ?`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListAssets query: %w", err)
	}
	defer rows.Close()
	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)
	return assets, nil
}

// listAssetsByIDs fetches the given assets and preserves the caller's order
// (newest-first from the spot cluster), since SQL IN(...) does not guarantee it.
func (s *SearchService) listAssetsByIDs(userID string, ids []string) ([]Asset, error) {
	placeholders := make([]string, len(ids))
	args := []any{userID}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	q := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
  AND a.id IN (` + strings.Join(placeholders, ",") + `)`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("listAssetsByIDs query: %w", err)
	}
	defer rows.Close()
	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}

	// Restore the caller's order (the cluster's newest-first sequence).
	pos := make(map[string]int, len(ids))
	for i, id := range ids {
		pos[id] = i
	}
	sort.Slice(assets, func(i, j int) bool { return pos[assets[i].ID] < pos[assets[j].ID] })

	enrichPlaceNames(s.db, assets)
	return assets, nil
}

// GetAsset returns a single asset by ID; returns ErrNotFound when absent.
func (s *SearchService) GetAsset(userID, id string) (*Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
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

// UpdateDurationMs persists a (re-probed) duration back onto the asset row,
// repairing historical rows whose duration_ms was 0/missing at ingest time.
func (s *SearchService) UpdateDurationMs(id string, ms int64) error {
	_, err := s.db.Exec(`UPDATE assets SET duration_ms=? WHERE id=?`, ms, id)
	return err
}

// DeleteAsset removes an asset by ID; returns ErrNotFound when absent.
func (s *SearchService) DeleteAsset(id string) error {
	dropClipVector(s.db, id) // before the cascade drops asset_clip_idx
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
