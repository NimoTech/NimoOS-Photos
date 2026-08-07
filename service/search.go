package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
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

	// onCaptionDelete is called after DeleteAsset (the hard-delete call site
	// right next to dropClipVector) succeeds, wired up for
	// CaptionFeeder.DeleteRemote (Task 4). Injected as a function field to
	// avoid SearchService depending directly on the CaptionFeeder type; safely
	// skipped when nil.
	onCaptionDelete func(assetID string)
}

// NewSearchService constructs a SearchService.
// ml may be nil when only non-CLIP methods (Timeline, ListAssets, …) are used.
func NewSearchService(db *sql.DB, ml textEmbedder) *SearchService {
	return &SearchService{db: db, ml: ml}
}

// SetCaptionDelete injects the caption-deletion callback invoked after an
// asset's hard delete succeeds (typically CaptionFeeder.DeleteRemote).
func (s *SearchService) SetCaptionDelete(fn func(assetID string)) {
	s.onCaptionDelete = fn
}

// ─── Smart (CLIP) search ─────────────────────────────────────────────────────

// minMatchSimilarity returns the configured relevance floor (photos.MinMatchSimilarity
// in the config file; config.Config.MinMatchSimilarity). When > 0, SmartSearch drops
// results whose cosine similarity is below it. It is compared against
// Asset.MatchScore, which since scan.go's displayScore is the *recalibrated*
// [0,1] display score — not the raw CLIP cosine — so this threshold's semantics
// are display-scale, not raw-cosine-scale.
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
// Defaults to 0 (no filtering) when unset in the config file or when config is
// not initialized (e.g. tests constructing SearchService directly) — current
// behaviour (and value) is unchanged by this recalibration. Set
// photos.MinMatchSimilarity in the config file to enable a relevance floor.
func minMatchSimilarity() float64 {
	if config.Cfg != nil {
		return config.Cfg.MinMatchSimilarity
	}
	return 0.0
}

// maxSmartSearchK is the hard ceiling on the KNN k value regardless of how
// deep a page offset+limit requests, or how much the over-fetch retry loop
// below widens it — a global backstop against pathological pagination
// requests hammering sqlite-vec with an unbounded k.
const maxSmartSearchK = 2000

// defaultSmartSearchSlack is the initial over-fetch margin added on top of
// offset+limit when computing the KNN k. sqlite-vec's "k = ?" clause only
// bounds the vector candidate pool — it is evaluated before this query's own
// WHERE filters (deleted_at IS NULL, offline = 0, is_live_photo_video = 0,
// year/month) run. Any KNN candidate inside the k-window that gets dropped by
// those filters (e.g. an asset moved to the trash, which keeps its CLIP
// vector) silently shrinks the page below limit. See SmartSearch's retry
// loop, which widens k (doubling this slack) until the filtered sequence
// covers the page or the library is genuinely exhausted.
const defaultSmartSearchSlack = 16

// SmartSearch performs KNN vector search on CLIP embeddings filtered by optional
// year/month, returning up to limit results starting at offset (0-based).
//
// limit is a page size, not a hard top-k: internally the KNN k starts at
// offset+limit+defaultSmartSearchSlack (capped at maxSmartSearchK) and is
// doubled and re-queried whenever the filtered sequence still falls short of
// offset+limit and the raw (pre-similarity-filter) candidate count is still
// growing — see the over-fetch retry loop below. This guarantees every page
// is filled to limit as long as enough live candidates exist anywhere in the
// ranking, even when candidates inside the window get filtered out by
// deleted_at/offline/is_live_photo_video. The page is then sliced out of
// that same globally-ordered, filtered sequence, so pages never gap or
// overlap. OCR merging and adaptive cut tiering (applyCutTiering) only run
// on the first page (offset==0) — best matches naturally live at the head of
// the ranking. Deeper pages (offset>0) skip OCR merging entirely and have
// every result marked BelowCut=true: a page beyond the first is by
// definition part of the "more results" tier.
//
// offset/limit paginate the SEMANTIC sequence only; OCR does not occupy a
// semantic slot. The first-page response is therefore deduped-OCR-first +
// semantic[0:limit], and its total length may exceed limit (up to ~2×limit)
// — see mergeOCRFirst's doc. Deep pages still slice the semantic ranking
// starting at offset, so this is what keeps a semantic entry from being
// permanently lost when an OCR hit would otherwise have pushed it out of the
// first page.
func (s *SearchService) SmartSearch(query string, limit int, offset int, filters SearchFilters) ([]Asset, error) {
	if offset < 0 {
		offset = 0
	}
	need := offset + limit

	queryVec, err := s.ml.CLIPTextEmbed(query)
	if err != nil {
		return nil, fmt.Errorf("SmartSearch embed: %w", err)
	}
	blob := sqlite.SerializeFloat32(queryVec)

	slack := defaultSmartSearchSlack
	k := need + slack
	if k > maxSmartSearchK {
		k = maxSmartSearchK
	}

	// Over-fetch and retry until the filtered sequence covers the page, the
	// global k cap is reached, or the raw candidate count stops growing
	// (widening k further cannot surface more rows — the vector table itself
	// is exhausted, not just filtered down, so this is the true bottom of
	// the library rather than a filtering artifact).
	var assets []Asset
	prevRaw := -1
	for {
		raw, filtered, err := s.knnSemanticFetch(blob, k, filters)
		if err != nil {
			return nil, err
		}
		assets = filtered
		if len(filtered) >= need || k >= maxSmartSearchK {
			break
		}
		if len(raw) == prevRaw {
			break
		}
		prevRaw = len(raw)
		slack *= 2
		k = need + slack
		if k > maxSmartSearchK {
			k = maxSmartSearchK
		}
	}

	// Slice the page out of the over-fetched, filtered, globally-ordered
	// semantic sequence. A short slice here (fewer than limit) only happens
	// when the loop above concluded the library is genuinely exhausted.
	var page []Asset
	if offset >= len(assets) {
		page = assets[:0]
	} else {
		end := offset + limit
		if end > len(assets) {
			end = len(assets)
		}
		page = assets[offset:end]
	}

	if offset == 0 {
		// Exact-text OCR hits outrank semantic guesses: prepend them at score
		// 1.0 and drop the CLIP duplicate when the same asset matched both
		// ways. Only meaningful on the first page — OCR hits are pinned to
		// the head of the ranking, which deeper pages never reach.
		//
		// OCR does NOT occupy a semantic slot: offset/limit apply only to the
		// semantic sequence (already limit-sized here — page is sliced to
		// [0:limit] above). mergeOCRFirst only dedupes and prepends — it
		// must not truncate the merged result back to limit, or an OCR hit
		// would push the last semantic entry out of the first page while
		// deep pages still slice the *semantic* ranking starting at
		// `offset`, permanently losing that entry (found in review; see the
		// design doc's "incremental correction" section and
		// TestSmartSearchOCRDoesNotDisplaceSemanticAcrossPages).
		if filters.IncludeOCR {
			ocrHits, err := s.ocrSearch(query, limit, filters)
			if err != nil {
				return nil, err
			}
			page = mergeOCRFirst(ocrHits, page)
		}

		// Adaptive cut: mark the long tail of the semantic-hit subsequence as
		// BelowCut so the client can fold it into a "more results" tier. OCR
		// hits are excluded from the computation and always stay in the best
		// tier (see applyCutTiering's doc).
		applyCutTiering(page)
	} else {
		// Deep page: every deep-page result is definitionally part of the
		// folded "more results" tier. A library shorter than offset+limit
		// naturally yields fewer than limit results here.
		for i := range page {
			page[i].BelowCut = true
		}
	}

	// Attach named persons so the client can offer a People filter on results.
	if err := s.attachNamedFaces(page); err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, page)
	return page, nil
}

// SearchAssetsByText is the minimal pathway reused by the Smart Moments theme
// engine (service/moments_theme.go): pure "text encode + CLIP vector KNN",
// extracted from SmartSearch's upper half (this file's CLIPTextEmbed +
// serialization around lines 174-184, plus knnSemanticFetch below).
// Deliberately does not apply SmartSearch's other policies — OCR merging,
// adaptive cut tiering, the global MinMatchSimilarity floor — those are
// strategies for interactive search result display; the theme engine
// filters by its own recipe's MinScore threshold instead, a different
// semantic that shouldn't be mixed in. Only structural filtering (is_live_
// photo_video/deleted_at/offline, the raw sequence already built into
// knnSemanticFetch) plus returning the top-K by distance order — unaffected
// by any of SmartSearch's own tests (not a single line of SmartSearch/
// knnSemanticFetch was changed).
func (s *SearchService) SearchAssetsByText(ctx context.Context, prompt string, topK int) ([]AssetScore, error) {
	queryVec, err := s.ml.CLIPTextEmbed(prompt)
	if err != nil {
		return nil, fmt.Errorf("SearchAssetsByText embed: %w", err)
	}
	blob := sqlite.SerializeFloat32(queryVec)

	raw, _, err := s.knnSemanticFetch(blob, topK, SearchFilters{})
	if err != nil {
		return nil, err
	}
	out := make([]AssetScore, 0, len(raw))
	for _, a := range raw {
		if a.MatchScore == nil {
			continue
		}
		out = append(out, AssetScore{AssetID: a.ID, Score: *a.MatchScore})
	}
	return out, nil
}

// knnSemanticFetch runs the base KNN query for k neighbours plus the shared
// structural filters (is_live_photo_video, deleted_at, offline, year/month —
// all evaluated in SQL) and tags every hit MatchedBy="semantic". It returns
// both the raw (structurally-filtered only) and filtered (additionally
// passed through MinMatchSimilarity) sequences: SmartSearch's retry loop
// compares raw counts across widening k to tell "the vector table has no
// more candidates" (raw stops growing — true exhaustion) apart from "minSim
// trimmed low-similarity extras" (raw keeps growing but filtered doesn't
// reach the page — widening k is still worth trying, up to the global cap).
func (s *SearchService) knnSemanticFetch(blob []byte, k int, filters SearchFilters) (raw, filtered []Asset, err error) {
	// Base KNN query — sqlite-vec uses "MATCH ? AND k = ?" syntax.
	// asset_exif is LEFT JOINed for latitude/longitude so the client can offer a
	// Places filter on results (reverse-geocoded to country in the UI).
	baseSQL := `
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
       a.indexed_at, a.status, e.latitude, e.longitude, vec.distance
FROM clip_embeddings AS vec
JOIN asset_clip_idx AS idx ON idx.rowid = vec.rowid
JOIN assets a ON a.id = idx.asset_id
LEFT JOIN asset_exif AS e ON e.asset_id = a.id
WHERE vec.embedding MATCH ? AND k = ?
  AND a.is_live_photo_video = 0
  AND a.deleted_at IS NULL AND a.offline = 0`

	args := []any{blob, k}

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
		return nil, nil, fmt.Errorf("SmartSearch query: %w", err)
	}
	defer rows.Close()

	raw, err = scanSearchAssets(rows)
	if err != nil {
		return nil, nil, err
	}
	// Mark these as pure-CLIP semantic hits. If IncludeOCR is set, mergeOCRFirst
	// later drops any asset that also has an OCR hit and keeps the OCR entry
	// (already tagged "ocr" in ocrSearch), so this tag only survives on assets
	// that matched by semantic similarity alone.
	for i := range raw {
		raw[i].MatchedBy = "semantic"
	}

	minSim := minMatchSimilarity()
	if minSim <= 0 {
		return raw, raw, nil
	}
	kept := make([]Asset, 0, len(raw))
	for _, a := range raw {
		if a.MatchScore == nil || *a.MatchScore >= minSim {
			kept = append(kept, a)
		}
	}
	return raw, kept, nil
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
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

// OCRLineHit is one stored OCR line of an asset, exposed by GET
// /assets/:id/ocr for search-hit highlighting. Box is the raw normalized
// quadrilateral ([x1,y1,…,x4,y4] in [0,1] of the image dimensions, straight
// from immich-ml); empty when the ML service omitted geometry.
type OCRLineHit struct {
	Text  string    `json:"text"`
	Box   []float64 `json:"box"`
	Score float64   `json:"score"`
}

// OCRLines returns an asset's stored OCR lines ordered by line_no. A
// non-empty query filters to lines containing it as a case-insensitive
// substring — deliberately the same instr(lower(),lower()) rule as
// ocrSearch, so every search hit is guaranteed to highlight. Returns
// ErrNotFound when the asset does not exist (or is trashed); assets whose
// OCR predates line storage (boxes_ver=0) simply yield an empty slice until
// the backfill reaches them.
func (s *SearchService) OCRLines(assetID, query string) ([]OCRLineHit, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM assets WHERE id = ? AND deleted_at IS NULL`, assetID).Scan(&one)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("OCRLines asset lookup: %w", err)
	}

	q := `SELECT text, box, COALESCE(score, 0) FROM asset_ocr_lines WHERE asset_id = ?`
	args := []any{assetID}
	if t := strings.TrimSpace(query); t != "" {
		q += ` AND instr(lower(text), lower(?)) > 0`
		args = append(args, t)
	}
	q += ` ORDER BY line_no`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("OCRLines query: %w", err)
	}
	defer rows.Close()

	out := make([]OCRLineHit, 0, 8)
	for rows.Next() {
		var h OCRLineHit
		var boxJSON string
		if err := rows.Scan(&h.Text, &boxJSON, &h.Score); err != nil {
			return nil, fmt.Errorf("OCRLines scan: %w", err)
		}
		if err := json.Unmarshal([]byte(boxJSON), &h.Box); err != nil || h.Box == nil {
			h.Box = []float64{}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// mergeOCRFirst concatenates ocr hits before clip results, dropping duplicate
// asset IDs (the OCR entry wins). It deliberately does NOT trim the result to
// any limit: OCR hits must not occupy semantic pagination slots, so the
// merged length may exceed limit (bounded by len(ocr)+len(clip), both already
// individually capped by their callers). See SmartSearch's offset==0 branch
// for the pagination-correctness rationale.
func mergeOCRFirst(ocr, clip []Asset) []Asset {
	seen := make(map[string]struct{}, len(ocr)+len(clip))
	out := make([]Asset, 0, len(ocr)+len(clip))
	for _, a := range append(ocr, clip...) {
		if _, dup := seen[a.ID]; dup {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
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

// attachNamedFacesAll fills Faces like attachNamedFaces but with a single
// full-scan query and no IN(...) — Timeline passes the entire library and
// would otherwise approach SQLite's bound-parameter limit as it grows.
func (s *SearchService) attachNamedFacesAll(assets []Asset) error {
	if len(assets) == 0 {
		return nil
	}
	idIndex := make(map[string]int, len(assets))
	for i, a := range assets {
		idIndex[a.ID] = i
	}
	rows, err := s.db.Query(`
SELECT fd.asset_id, p.name
FROM face_detections fd
JOIN face_person fp ON fp.face_id = fd.id
JOIN persons p ON p.id = fp.person_id
WHERE p.name <> '' AND COALESCE(p.hidden, 0) = 0 AND COALESCE(fd.excluded, 0) = 0`)
	if err != nil {
		return fmt.Errorf("attachNamedFacesAll query: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]map[string]bool)
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, `+hasOcrExpr+`,
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
	if err := s.attachNamedFacesAll(assets); err != nil {
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, `+hasOcrExpr+`,
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
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	return assets, nil
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
	if fromID == intoID {
		return fmt.Errorf("MergePersons: from and into must be different persons")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("MergePersons begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var one int
	if err = tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, fromID).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("MergePersons check from: %w", err)
	}
	if err = tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, intoID).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("MergePersons check into: %w", err)
	}

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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
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
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
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
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
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
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	return assets, nil
}

// GetAsset returns a single asset by ID; returns ErrNotFound when absent.
func (s *SearchService) GetAsset(userID, id string) (*Asset, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, `+hasOcrExpr+`,
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
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
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
	if s.onCaptionDelete != nil {
		s.onCaptionDelete(id) // caption cascade: prevents the agent from retrieving ghost results
	}
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
