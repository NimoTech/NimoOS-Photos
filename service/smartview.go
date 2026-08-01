package service

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type SmartView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	CreatedAt     time.Time  `json:"createdAt"`
	Description   string     `json:"description,omitempty"`
	Conds         []string   `json:"conds"`
	Threshold     int        `json:"threshold"`
	Live          bool       `json:"live"`
	IncludeVideos bool       `json:"includeVideos"`
	Count         int        `json:"count"`
	AddedThisWeek int        `json:"addedThisWeek"`
	Seeds         []string   `json:"seeds"`
	Median        int        `json:"median,omitempty"`
	StorageBytes  int64      `json:"storageBytes,omitempty"`
	Distribution  []int      `json:"distribution,omitempty"`
	EvaluatedAt   *time.Time `json:"evaluatedAt,omitempty"`
}

type SmartViewInput struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	CondsRaw      []string `json:"condsRaw"`
	Threshold     int      `json:"threshold"`
	Live          bool     `json:"live"`
	IncludeVideos bool     `json:"includeVideos"`
}

type SmartViewPatch struct {
	Name          *string   `json:"name,omitempty"`
	Description   *string   `json:"description,omitempty"`
	CondsRaw      *[]string `json:"condsRaw,omitempty"`
	Threshold     *int      `json:"threshold,omitempty"`
	Live          *bool     `json:"live,omitempty"`
	IncludeVideos *bool     `json:"includeVideos,omitempty"`
}

type SmartViewService struct {
	db     *sql.DB
	search *SearchService
}

func NewSmartViewService(db *sql.DB, search *SearchService) *SmartViewService {
	return &SmartViewService{db: db, search: search}
}

func (s *SmartViewService) Create(in SmartViewInput) (*SmartView, error) {
	if err := s.insertRow(&in); err != nil {
		return nil, err
	}
	s.logActivity(in.ID, "created", "", nil)
	if err := s.Evaluate(in.ID); err != nil {
		return nil, err
	}
	return s.Get(in.ID)
}

// insertRow only persists the smart_views row (doesn't trigger Evaluate/
// logging), shared by Create and the conversion endpoint (ConvertFromAlbum)
// for the same row-creation logic, avoiding a duplicate evaluation in the
// conversion flow.
func (s *SmartViewService) insertRow(in *SmartViewInput) error {
	if in.ID == "" || in.Name == "" {
		return ErrInvalidInput
	}
	if in.Threshold < 50 || in.Threshold > 99 {
		in.Threshold = 70
	}
	rawJSON, _ := json.Marshal(in.CondsRaw)
	parsed := parseWithDescFallback(s.db, in.CondsRaw, in.Description)
	parsedJSON, _ := json.Marshal(parsed)
	_, err := s.db.Exec(`INSERT INTO smart_views
		(id,name,description,conds_raw,conds_parsed,threshold,live,include_videos)
		VALUES(?,?,?,?,?,?,?,?)`,
		in.ID, in.Name, in.Description, string(rawJSON), string(parsedJSON),
		in.Threshold, boolToInt(in.Live), boolToInt(in.IncludeVideos))
	return err
}

func (s *SmartViewService) List() ([]SmartView, error) {
	rows, err := s.db.Query(`SELECT id FROM smart_views ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
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
	out := make([]SmartView, 0, len(ids))
	for _, id := range ids {
		sv, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *sv)
	}
	return out, nil
}

func (s *SmartViewService) Get(id string) (*SmartView, error) {
	var (
		sv          SmartView
		rawJSON     string
		liveI, vidI int
		evaluatedAt sql.NullTime
		desc        sql.NullString
	)
	err := s.db.QueryRow(`SELECT id,name,description,conds_raw,threshold,live,include_videos,evaluated_at,created_at
		FROM smart_views WHERE id=?`, id).Scan(
		&sv.ID, &sv.Name, &desc, &rawJSON, &sv.Threshold, &liveI, &vidI, &evaluatedAt, &sv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sv.Description = desc.String
	sv.Live = liveI == 1
	sv.IncludeVideos = vidI == 1
	if evaluatedAt.Valid {
		sv.EvaluatedAt = &evaluatedAt.Time
	}
	_ = json.Unmarshal([]byte(rawJSON), &sv.Conds)
	if sv.Conds == nil {
		sv.Conds = []string{}
	}
	s.fillStats(&sv)
	return &sv, nil
}

func (s *SmartViewService) Update(id string, p SmartViewPatch) (*SmartView, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	sets := []string{}
	args := []any{}
	if p.Name != nil {
		sets = append(sets, "name=?")
		args = append(args, *p.Name)
	}
	if p.Description != nil {
		sets = append(sets, "description=?")
		args = append(args, *p.Description)
	}
	if p.Threshold != nil {
		sets = append(sets, "threshold=?")
		args = append(args, *p.Threshold)
	}
	if p.Live != nil {
		sets = append(sets, "live=?")
		args = append(args, boolToInt(*p.Live))
	}
	if p.IncludeVideos != nil {
		sets = append(sets, "include_videos=?")
		args = append(args, boolToInt(*p.IncludeVideos))
	}
	// conds_parsed is NOT written here — Evaluate (triggered below for any
	// match-affecting patch) re-parses from conds_raw + description and
	// refreshes the snapshot itself.
	if p.CondsRaw != nil {
		rawJSON, _ := json.Marshal(*p.CondsRaw)
		sets = append(sets, "conds_raw=?")
		args = append(args, string(rawJSON))
	}
	sets = append(sets, "updated_at=CURRENT_TIMESTAMP")
	if len(sets) > 0 {
		q := "UPDATE smart_views SET " + joinComma(sets) + " WHERE id=?"
		args = append(args, id)
		if _, err := s.db.Exec(q, args...); err != nil {
			return nil, err
		}
	}
	s.logActivity(id, "updated", "", nil)
	// Resuming live (unpausing) also needs a recompute: while paused, the
	// displayScore calibration endpoints (simDisplayFloor/Ceil) may have shifted
	// with a model changeover, so the old match_score is on the old scale.
	resumed := p.Live != nil && *p.Live
	if p.CondsRaw != nil || p.Description != nil || p.Threshold != nil || p.IncludeVideos != nil || resumed {
		if err := s.Evaluate(id); err != nil {
			return nil, err
		}
	}
	return s.Get(id)
}

func (s *SmartViewService) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM smart_views WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SmartViewService) fillStats(sv *SmartView) {
	sv.Seeds = []string{}
	sv.Distribution = make([]int, 10)

	_ = s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id=? AND origin<>2`, sv.ID).Scan(&sv.Count)

	_ = s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches
		WHERE smart_view_id=? AND origin<>2 AND matched_at >= datetime('now','-7 days')`, sv.ID).Scan(&sv.AddedThisWeek)

	_ = s.db.QueryRow(`SELECT COALESCE(SUM(a.file_size),0) FROM smart_view_matches m
		JOIN assets a ON a.id=m.asset_id WHERE m.smart_view_id=? AND m.origin<>2`, sv.ID).Scan(&sv.StorageBytes)

	rows, err := s.db.Query(`SELECT match_score FROM smart_view_matches WHERE smart_view_id=? AND origin<>2 ORDER BY match_score`, sv.ID)
	if err == nil {
		var scores []float64
		for rows.Next() {
			var sc float64
			rows.Scan(&sc)
			scores = append(scores, sc)
			bucket := int(sc * 10)
			if bucket > 9 {
				bucket = 9
			}
			if bucket < 0 {
				bucket = 0
			}
			sv.Distribution[bucket]++
		}
		rows.Close()
		if n := len(scores); n > 0 {
			med := scores[n/2]
			if n%2 == 0 {
				med = (scores[n/2-1] + scores[n/2]) / 2
			}
			sv.Median = int(med*100 + 0.5)
		}
	}

	// The Seeds preview shows high-aesthetic-score assets first (NULL sorts
	// last), with matches ordered by match score within the same aesthetic tier.
	srows, err := s.db.Query(`SELECT m.asset_id FROM smart_view_matches m
		JOIN assets a ON a.id = m.asset_id
		WHERE m.smart_view_id=? AND m.origin<>2
		ORDER BY (a.aesthetic_score IS NULL) ASC, a.aesthetic_score DESC,
		         m.match_score DESC LIMIT 6`, sv.ID)
	if err == nil {
		for srows.Next() {
			var aid string
			srows.Scan(&aid)
			sv.Seeds = append(sv.Seeds, aid)
		}
		srows.Close()
	}
}

// MatchedAssets returns paginated matched assets, optionally only those added
// in the last 7 days (recent=true), ordered by score then recency.
// IsNew is per-user: true until userID opens the asset after it matched
// (asset_views row at/after matched_at) — drives the dismissible "New" tag.
func (s *SmartViewService) MatchedAssets(id string, limit, offset int, recent bool, userID string) ([]Asset, error) {
	// origin<>2 hides manually excluded rows (the memory of them stays in the
	// store, but they're never visible on any read path).
	where := `m.smart_view_id=? AND m.origin<>2`
	args := []any{userID, id}
	if recent {
		where += ` AND m.matched_at >= datetime('now','-7 days')`
	}
	// julianday() tolerates both "YYYY-MM-DD HH:MM:SS" (CURRENT_TIMESTAMP) and
	// ISO "T...Z" / "+00:00" (driver-written time.Time) datetime encodings.
	q := `SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
	       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
	       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, ` + hasOcrExpr + `,
	       a.indexed_at, a.status, m.match_score, m.origin,
	       (v.last_viewed_at IS NULL OR julianday(v.last_viewed_at) < julianday(m.matched_at))
	FROM smart_view_matches m JOIN assets a ON a.id=m.asset_id
	LEFT JOIN asset_views v ON v.user_id=? AND v.asset_id=a.id
	WHERE ` + where + ` AND a.deleted_at IS NULL AND a.offline = 0
	ORDER BY m.match_score DESC, COALESCE(a.taken_at,a.indexed_at) DESC
	LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		var score float64
		var origin int
		// duration_ms / taken_at / file_size / indexed_at are nullable in the
		// assets table — scan through sql.Null* like scanAssets does, or a single
		// NULL (every photo's duration_ms) fails the whole query.
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		if err := rows.Scan(&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
			&indexedAt, &a.Status, &score, &origin, &a.IsNew); err != nil {
			return nil, err
		}
		a.Pinned = origin == 1
		if fileSize.Valid {
			a.FileSize = fileSize.Int64
		}
		if takenAt.Valid {
			t := takenAt.Time
			a.TakenAt = &t
		}
		if durationMs.Valid {
			a.DurationMs = durationMs.Int64
		}
		if indexedAt.Valid {
			t := indexedAt.Time
			a.IndexedAt = &t
		}
		a.MatchScore = &score
		out = append(out, a)
	}
	return out, nil
}

// PinAssets pins the given assets into the view: a recompute can't wash them
// out, only the user can remove them. Score is always 1.0 (consistent with
// the existing semantics for pure structural conditions), matched_at=now
// makes IsNew take effect. An invalid asset (doesn't exist/soft-deleted) is
// silently skipped; re-pinning is idempotent and doesn't count again.
func (s *SmartViewService) PinAssets(id string, assetIDs []string) (int, error) {
	var dummy string
	err := s.db.QueryRow(`SELECT id FROM smart_views WHERE id=?`, id).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	added := 0
	for _, aid := range assetIDs {
		var ok string
		if err := tx.QueryRow(`SELECT id FROM assets WHERE id=? AND deleted_at IS NULL`, aid).Scan(&ok); err != nil {
			continue // silently skip an invalid asset
		}
		var org int
		err := tx.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, id, aid).Scan(&org)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES(?,?,1.0,1)`,
				id, aid); err != nil {
				return 0, err
			}
			added++
		case err != nil:
			return 0, err
		case org != 1: // an auto row gets upgraded to pinned / an excluded row flips back to pinned
			if _, err := tx.Exec(`UPDATE smart_view_matches SET origin=1, match_score=1.0 WHERE smart_view_id=? AND asset_id=?`,
				id, aid); err != nil {
				return 0, err
			}
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// RemoveAssets removes in tiers: a pinned row = unpin (delete the row; after
// a recompute, if it still naturally matches it comes back as a regular
// match); an auto row = set to excluded (origin=2); an already-excluded row
// or an id not in the table is a no-op.
// When any unpin or exclude happens and the view is live, synchronously
// triggers a recompute so the grid immediately reflects the true state.
func (s *SmartViewService) RemoveAssets(id string, assetIDs []string) (int, int, error) {
	var live int
	err := s.db.QueryRow(`SELECT live FROM smart_views WHERE id=?`, id).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	unpinned, excluded := 0, 0
	for _, aid := range assetIDs {
		var org int
		err := tx.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, id, aid).Scan(&org)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		switch org {
		case 1:
			if _, err := tx.Exec(`DELETE FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, id, aid); err != nil {
				return 0, 0, err
			}
			unpinned++
		case 0:
			if _, err := tx.Exec(`UPDATE smart_view_matches SET origin=2 WHERE smart_view_id=? AND asset_id=?`, id, aid); err != nil {
				return 0, 0, err
			}
			excluded++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	// If an unpinned photo still naturally matches, a recompute brings it back
	// as origin=0; a paused view is skipped (paused = no recompute).
	if unpinned > 0 && live == 1 {
		if err := s.Evaluate(id); err != nil {
			zap.L().Warn("smartview: recompute after remove failed", zap.String("id", id), zap.Error(err))
		}
	}
	return unpinned, excluded, nil
}

// RestoreAssets restores excluded assets: deletes the excluded row; a live
// view triggers a recompute so it naturally comes back.
func (s *SmartViewService) RestoreAssets(id string, assetIDs []string) (int, error) {
	var live int
	err := s.db.QueryRow(`SELECT live FROM smart_views WHERE id=?`, id).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	restored := 0
	for _, aid := range assetIDs {
		res, err := s.db.Exec(`DELETE FROM smart_view_matches WHERE smart_view_id=? AND asset_id=? AND origin=2`, id, aid)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			restored++
		}
	}
	if restored > 0 && live == 1 {
		if err := s.Evaluate(id); err != nil {
			zap.L().Warn("smartview: recompute after restore failed", zap.String("id", id), zap.Error(err))
		}
	}
	return restored, nil
}

// ExcludedAssets returns the view's exclusion list (for the detail page's
// collapsed section); the volume is small so it isn't paginated.
func (s *SmartViewService) ExcludedAssets(id string) ([]Asset, error) {
	rows, err := s.db.Query(`SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
	       COALESCE(a.original_name,''), a.taken_at, a.duration_ms
	FROM smart_view_matches m JOIN assets a ON a.id=m.asset_id
	WHERE m.smart_view_id=? AND m.origin=2 AND a.deleted_at IS NULL AND a.offline=0
	ORDER BY COALESCE(a.taken_at,a.indexed_at) DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		var takenAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		if err := rows.Scan(&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs); err != nil {
			return nil, err
		}
		if fileSize.Valid {
			a.FileSize = fileSize.Int64
		}
		if takenAt.Valid {
			t := takenAt.Time
			a.TakenAt = &t
		}
		if durationMs.Valid {
			a.DurationMs = durationMs.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SmartViewService) logActivity(svID, evType, detail string, assetIDs []string) {
	var idsJSON string
	if len(assetIDs) > 0 {
		b, _ := json.Marshal(assetIDs)
		idsJSON = string(b)
	}
	_, _ = s.db.Exec(`INSERT INTO smart_view_activity(id,smart_view_id,event_type,detail,asset_ids)
		VALUES(?,?,?,?,?)`, newSVID("ev"), svID, evType, detail, idsJSON)
}

func newSVID(prefix string) string { return prefix + "-" + uuid.NewString() }

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// Evaluate runs a full match evaluation and reconciles smart_view_matches.
// Conditions are re-parsed from conds_raw on every run (not read from the
// conds_parsed snapshot): parser upgrades then apply to existing views, and
// relative date windows like "last 30 days" stay anchored to now instead of
// being frozen at creation time. conds_parsed is refreshed as a side effect
// so API consumers keep seeing the current interpretation.
func (s *SmartViewService) Evaluate(id string) error {
	var rawJSON string
	var desc sql.NullString
	var includeVideos, threshold int
	err := s.db.QueryRow(`SELECT conds_raw, description, include_videos, threshold FROM smart_views WHERE id=?`, id).
		Scan(&rawJSON, &desc, &includeVideos, &threshold)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var raw []string
	_ = json.Unmarshal([]byte(rawJSON), &raw)
	parsed := parseWithDescFallback(s.db, raw, desc.String)
	if parsedJSON, jerr := json.Marshal(parsed); jerr == nil {
		_, _ = s.db.Exec(`UPDATE smart_views SET conds_parsed=? WHERE id=?`, string(parsedJSON), id)
	}

	includeVideosBool := includeVideos == 1
	scoreMap, err := s.evalParsed(parsed, threshold, !includeVideosBool)
	if err != nil {
		return err
	}
	type scored struct {
		id    string
		score float64
	}
	keep := make([]scored, 0, len(scoreMap))
	for aid, sc := range scoreMap {
		keep = append(keep, scored{aid, sc})
	}
	existing := map[string]int{} // asset_id → origin (0=auto/1=pinned/2=excluded)
	rows, err := s.db.Query(`SELECT asset_id, origin FROM smart_view_matches WHERE smart_view_id=?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var aid string
		var org int
		rows.Scan(&aid, &org)
		existing[aid] = org
	}
	rows.Close()

	keepSet := map[string]struct{}{}
	var added []string
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, k := range keep {
		keepSet[k.id] = struct{}{}
		if org, ok := existing[k.id]; ok {
			// A manual row (pinned/excluded) is never touched: a pinned score is
			// always 1.0, an excluded row keeps its "memory".
			if org != 0 {
				continue
			}
			if _, err := tx.Exec(`UPDATE smart_view_matches SET match_score=? WHERE smart_view_id=? AND asset_id=?`,
				k.score, id, k.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score) VALUES(?,?,?)`,
				id, k.id, k.score); err != nil {
				return err
			}
			added = append(added, k.id)
		}
	}
	for aid, org := range existing {
		if org != 0 {
			continue // only the user can move a pinned/excluded row; a recompute never deletes one
		}
		if _, ok := keepSet[aid]; !ok {
			if _, err := tx.Exec(`DELETE FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, id, aid); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(added) > 0 {
		s.logActivity(id, "matched", "", added)
	}
	s.touchEvaluated(id)
	return nil
}

func (s *SmartViewService) touchEvaluated(id string) {
	_, _ = s.db.Exec(`UPDATE smart_views SET evaluated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
}

func (s *SmartViewService) assetsForPerson(personID string) (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT DISTINCT fd.asset_id
		FROM face_detections fd JOIN face_person fp ON fp.face_id=fd.id
		JOIN assets a ON a.id=fd.asset_id
		WHERE fp.person_id=? AND a.is_live_photo_video=0 AND a.deleted_at IS NULL AND a.offline=0`, personID)
	return scanIDSet(rows, err)
}

func (s *SmartViewService) assetsForPlace(text string) (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT DISTINCT g.asset_id FROM asset_geo g
		JOIN assets a ON a.id=g.asset_id
		WHERE (lower(g.city)=lower(?) OR lower(g.country)=lower(?) OR lower(g.region)=lower(?))
		  AND a.is_live_photo_video=0 AND a.deleted_at IS NULL AND a.offline=0`, text, text, text)
	return scanIDSet(rows, err)
}

func (s *SmartViewService) assetsForDateRange(start, end time.Time) (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT id FROM assets
		WHERE taken_at BETWEEN ? AND ? AND is_live_photo_video=0 AND deleted_at IS NULL AND offline=0`,
		start.UTC().Format("2006-01-02T15:04:05Z"), end.UTC().Format("2006-01-02T15:04:05Z"))
	return scanIDSet(rows, err)
}

// assetsForOCR matches assets whose recognized text contains any of the
// "|"-separated alternatives (case-insensitive substring). Like person/place/
// date, this is a structural condition: hit or miss, score 1.0.
func (s *SmartViewService) assetsForOCR(query string) (map[string]struct{}, error) {
	var conds []string
	var args []any
	for _, term := range strings.Split(query, "|") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		conds = append(conds, "instr(lower(o.text), lower(?)) > 0")
		args = append(args, term)
	}
	if len(conds) == 0 {
		return map[string]struct{}{}, nil
	}
	rows, err := s.db.Query(`SELECT o.asset_id FROM asset_ocr o
		JOIN assets a ON a.id=o.asset_id
		WHERE (`+strings.Join(conds, " OR ")+`)
		  AND a.is_live_photo_video=0 AND a.deleted_at IS NULL AND a.offline=0`, args...)
	return scanIDSet(rows, err)
}

func (s *SmartViewService) assetsForSemantic(query string) (map[string]float64, error) {
	results, err := s.search.SmartSearch(query, 500, 0, SearchFilters{})
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, a := range results {
		sc := 0.0
		if a.MatchScore != nil {
			sc = *a.MatchScore
		}
		out[a.ID] = sc
	}
	return out, nil
}

func (s *SmartViewService) excludeVideos(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return in
	}
	ph := make([]string, 0, len(in))
	args := make([]any, 0, len(in))
	for aid := range in {
		ph = append(ph, "?")
		args = append(args, aid)
	}
	q := `SELECT id FROM assets WHERE id IN (` + strings.Join(ph, ",") + `) AND mime_type LIKE 'video/%'`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return in
	}
	defer rows.Close()
	videos := map[string]struct{}{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		videos[id] = struct{}{}
	}
	out := map[string]struct{}{}
	for aid := range in {
		if _, isVid := videos[aid]; !isVid {
			out[aid] = struct{}{}
		}
	}
	return out
}

func scanIDSet(rows *sql.Rows, err error) (map[string]struct{}, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, nil
}

// SmartViewActivity is one activity log entry.
type SmartViewActivity struct {
	ID         string    `json:"id"`
	EventType  string    `json:"eventType"`
	Detail     string    `json:"detail,omitempty"`
	AssetIDs   []string  `json:"assetIds,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

func (s *SmartViewService) Duplicate(id string) (*SmartView, error) {
	var name, rawJSON, desc string
	var threshold, live, vid int
	err := s.db.QueryRow(`SELECT name,description,conds_raw,threshold,live,include_videos
		FROM smart_views WHERE id=?`, id).Scan(&name, &desc, &rawJSON, &threshold, &live, &vid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var conds []string
	_ = json.Unmarshal([]byte(rawJSON), &conds)
	return s.Create(SmartViewInput{
		ID:            newSVID("sv"),
		Name:          name + " (copy)",
		Description:   desc,
		CondsRaw:      conds,
		Threshold:     threshold,
		Live:          live == 1,
		IncludeVideos: vid == 1,
	})
}

func (s *SmartViewService) Activity(id string, limit int) ([]SmartViewActivity, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,event_type,COALESCE(detail,''),COALESCE(asset_ids,''),occurred_at
		FROM smart_view_activity WHERE smart_view_id=? ORDER BY occurred_at DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SmartViewActivity
	for rows.Next() {
		var a SmartViewActivity
		var idsJSON string
		if err := rows.Scan(&a.ID, &a.EventType, &a.Detail, &idsJSON, &a.OccurredAt); err != nil {
			return nil, err
		}
		if idsJSON != "" {
			_ = json.Unmarshal([]byte(idsJSON), &a.AssetIDs)
		}
		out = append(out, a)
	}
	return out, nil
}

// EvaluateAllLive re-evaluates all live smart views (paused skipped) to
// absorb newly indexed assets. Reuses Evaluate's reconcile (new matches get
// matched_at=now + a matched activity).
func (s *SmartViewService) EvaluateAllLive() error {
	rows, err := s.db.Query(`SELECT id FROM smart_views WHERE live=1`)
	if err != nil {
		return err
	}
	var liveIDs []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		liveIDs = append(liveIDs, id)
	}
	rows.Close()
	for _, svID := range liveIDs {
		if err := s.Evaluate(svID); err != nil {
			return err
		}
	}
	return nil
}

func intersect(sets []map[string]struct{}) map[string]struct{} {
	if len(sets) == 0 {
		return map[string]struct{}{}
	}
	base := sets[0]
	for _, s := range sets[1:] {
		if len(s) < len(base) {
			base = s
		}
	}
	out := map[string]struct{}{}
	for id := range base {
		in := true
		for _, s := range sets {
			if _, ok := s[id]; !ok {
				in = false
				break
			}
		}
		if in {
			out[id] = struct{}{}
		}
	}
	return out
}

// Preview evaluates condsRaw at threshold without persisting results.
// description participates the same way as in Create (semantic fallback).
// thresholdActive reports whether any semantic condition exists — without one
// every match scores a flat 1.0 and the threshold slider has no effect, which
// the UI uses to grey the slider out.
// Returns (totalMatched, topSeedIDs, thresholdActive, error).
func (s *SmartViewService) Preview(condsRaw []string, description string, threshold int, includeVideos bool) (int, []string, bool, error) {
	if threshold < 50 || threshold > 99 {
		threshold = 70
	}
	parsed := parseWithDescFallback(s.db, condsRaw, description)
	thresholdActive := false
	for _, c := range parsed {
		if c.Kind == condSemantic {
			thresholdActive = true
			break
		}
	}
	scored, err := s.evalParsed(parsed, threshold, !includeVideos)
	if err != nil {
		return 0, nil, false, err
	}
	ids := topNByScore(scored, 6)
	return len(scored), ids, thresholdActive, nil
}

// parseWithDescFallback parses condsRaw; when nothing executable comes out and
// the description is non-empty, the description itself is appended as a
// semantic (CLIP) condition so "What should Nimo match?" actually drives
// matching instead of being a stored-but-unread note.
func parseWithDescFallback(db *sql.DB, condsRaw []string, description string) []ParsedCond {
	parsed := ParseConditions(db, condsRaw)
	desc := strings.TrimSpace(description)
	if desc == "" {
		return parsed
	}
	for _, c := range parsed {
		if c.Kind != condUnsupported {
			return parsed
		}
	}
	return append(parsed, ParsedCond{Raw: desc, Kind: condSemantic, Value: desc})
}

// evalParsed is the shared matcher used by Evaluate (persisted) and Preview.
// excludeVideos=true drops video assets from the result set.
// Returns a map of assetID -> score.
func (s *SmartViewService) evalParsed(parsed []ParsedCond, threshold int, excludeVideos bool) (map[string]float64, error) {
	var sets []map[string]struct{}
	semanticScores := map[string][]float64{}
	hasExecutable := false
	for _, c := range parsed {
		switch c.Kind {
		case condPerson:
			ids, err := s.assetsForPerson(c.Value)
			if err != nil {
				return nil, err
			}
			sets = append(sets, ids)
			hasExecutable = true
		case condPlace:
			ids, err := s.assetsForPlace(c.Value)
			if err != nil {
				return nil, err
			}
			sets = append(sets, ids)
			hasExecutable = true
		case condDate:
			if c.Start != nil && c.End != nil {
				ids, err := s.assetsForDateRange(*c.Start, *c.End)
				if err != nil {
					return nil, err
				}
				sets = append(sets, ids)
				hasExecutable = true
			}
		case condOCR:
			ids, err := s.assetsForOCR(c.Value)
			if err != nil {
				return nil, err
			}
			sets = append(sets, ids)
			hasExecutable = true
		case condSemantic:
			scored, err := s.assetsForSemantic(c.Value)
			if err != nil {
				return nil, err
			}
			set := map[string]struct{}{}
			for aid, sc := range scored {
				set[aid] = struct{}{}
				semanticScores[aid] = append(semanticScores[aid], sc)
			}
			sets = append(sets, set)
			hasExecutable = true
		}
	}
	if !hasExecutable {
		return map[string]float64{}, nil
	}
	inter := intersect(sets)
	if excludeVideos {
		inter = s.excludeVideos(inter)
	}
	thr := float64(threshold) / 100.0
	out := map[string]float64{}
	for aid := range inter {
		score := 1.0
		if vs := semanticScores[aid]; len(vs) > 0 {
			sum := 0.0
			for _, v := range vs {
				sum += v
			}
			// SmartSearch's MatchScore is already the recalibrated [0,1] display
			// score (see displayScore in scan.go) — the single calibration layer
			// shared with the search UI's percentage. Use it as-is so the slider,
			// the stored match_score, the Median stat and the 50–100% distribution
			// chart all live on that one scale, with no second remapping here.
			score = sum / float64(len(vs))
		}
		if score >= thr {
			out[aid] = score
		}
	}
	return out, nil
}

// topNByScore returns the IDs of the top n entries in m by score (descending).
func topNByScore(m map[string]float64, n int) []string {
	type kv struct {
		id string
		sc float64
	}
	arr := make([]kv, 0, len(m))
	for id, sc := range m {
		arr = append(arr, kv{id, sc})
	}
	for i := 0; i < len(arr) && i < n; i++ {
		max := i
		for j := i + 1; j < len(arr); j++ {
			if arr[j].sc > arr[max].sc {
				max = j
			}
		}
		arr[i], arr[max] = arr[max], arr[i]
	}
	out := []string{}
	for i := 0; i < len(arr) && i < n; i++ {
		out = append(out, arr[i].id)
	}
	return out
}

// compensateDeleteSV is the cleanup step for ConvertFromAlbum's "create new,
// then delete old" failure rollback path: the primary error (the caller's
// original err) is always returned as usual; this just logs a failure of the
// compensating cleanup itself — previously it was silently swallowed
// (`_ = s.Delete(...)`), leaving a half-finished smart_view with no way to
// know about it or track it down.
func (s *SmartViewService) compensateDeleteSV(svID, stage string) {
	if err := s.Delete(svID); err != nil {
		zap.L().Warn("smartview: ConvertFromAlbum compensating cleanup failed, may leave a half-finished smart_view",
			zap.String("id", svID), zap.String("stage", stage), zap.Error(err))
	}
}

// compensateDeleteAlbum is the cleanup step for ConvertToAlbum's "create new,
// then delete old" failure rollback path, same semantics as
// compensateDeleteSV: logs a warn when the compensating cleanup itself fails,
// without changing the primary error returned to the caller.
func compensateDeleteAlbum(albumSvc *AlbumService, albumID, stage string) {
	if err := albumSvc.Delete(albumID); err != nil {
		zap.L().Warn("smartview: ConvertToAlbum compensating cleanup failed, may leave a half-finished album",
			zap.String("id", albumID), zap.String("stage", stage), zap.Error(err))
	}
}

// ConvertFromAlbumInput is the input for the "manual album → smart view"
// in-place conversion endpoint.
type ConvertFromAlbumInput struct {
	AlbumID       string
	Name          string
	Description   string
	CondsRaw      []string
	Threshold     int
	IncludeVideos bool
}

// ConvertFromAlbum turns a manual album in place into a smart view: creates
// a smart_views row (live=1) → locks all of the original album_assets as
// pinned members (in PinAssets's storage shape, origin=1/match_score=1.0) →
// synchronously triggers an Evaluate (same path as Create, pulling in theme
// matches) → deletes the original album (cascading album_assets).
//
// Transaction boundary: Evaluate internally calls semantic search (possibly
// an external ML call), which isn't a good fit for holding a lock inside one
// database transaction for the whole span; instead this uses a "create new,
// then delete old" order + cleans up the newly-created smart_views row on
// any failure, so no step's failure leaves a half-finished result.
func (s *SmartViewService) ConvertFromAlbum(in ConvertFromAlbumInput) (*SmartView, error) {
	albumSvc := NewAlbumService(s.db)
	album, err := albumSvc.Get(in.AlbumID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = album.Name
	}

	svID := newSVID("sv")
	svIn := SmartViewInput{
		ID:            svID,
		Name:          name,
		Description:   in.Description,
		CondsRaw:      in.CondsRaw,
		Threshold:     in.Threshold,
		Live:          true,
		IncludeVideos: in.IncludeVideos,
	}
	if err := s.insertRow(&svIn); err != nil {
		return nil, err
	}

	// All members of the original album (the full album_assets set, no
	// visibility filtering) are locked as pinned rows in PinAssets's storage
	// shape; invalid/soft-deleted assets are silently skipped by PinAssets.
	memberRows, err := s.db.Query(`SELECT asset_id FROM album_assets WHERE album_id=?`, in.AlbumID)
	if err != nil {
		s.compensateDeleteSV(svID, "query original album members failed")
		return nil, err
	}
	var memberIDs []string
	for memberRows.Next() {
		var aid string
		if err := memberRows.Scan(&aid); err != nil {
			memberRows.Close()
			s.compensateDeleteSV(svID, "scan original album members failed")
			return nil, err
		}
		memberIDs = append(memberIDs, aid)
	}
	memberRows.Close()
	if err := memberRows.Err(); err != nil {
		s.compensateDeleteSV(svID, "iterate original album members failed")
		return nil, err
	}
	if len(memberIDs) > 0 {
		if _, err := s.PinAssets(svID, memberIDs); err != nil {
			s.compensateDeleteSV(svID, "PinAssets failed")
			return nil, err
		}
	}

	// Synchronously triggers an Evaluate, consistent with Create's current
	// behavior — pulls in new photos matched by theme.
	if err := s.Evaluate(svID); err != nil {
		s.compensateDeleteSV(svID, "Evaluate failed")
		return nil, err
	}
	s.logActivity(svID, "converted_from_album", in.AlbumID, memberIDs)

	// The new identity is ready; delete the original manual album (cascading album_assets).
	if err := albumSvc.Delete(in.AlbumID); err != nil {
		s.compensateDeleteSV(svID, "delete original manual album failed")
		return nil, err
	}

	return s.Get(svID)
}

// ConvertToAlbum solidifies a smart view in place into a manual album:
// creates an albums row (name=sv.name) → writes current members (including
// pinned, excluding excluded) into album_assets sorted by score DESC (reusing
// MatchedAssets/ExportAsAlbum's existing data-fetching logic) → deletes the
// original smart_views row (cascading matches). A name collision follows the
// same 409 semantics as Export/regular Create (ErrAlbumNameExists, mapped by
// the route layer).
//
// Same transaction boundary as ConvertFromAlbum: create new, then delete old
// + clean up the already-created album on failure, leaving nothing
// half-finished.
func (s *SmartViewService) ConvertToAlbum(id string) (*Album, error) {
	sv, err := s.Get(id)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	albumSvc := NewAlbumService(s.db)
	album, err := albumSvc.Create(sv.Name)
	if err != nil {
		return nil, err
	}

	// Queries smart_view_matches directly rather than reusing MatchedAssets:
	// MatchedAssets is a read path that additionally filters a.offline=0 (an
	// offline asset isn't shown while its removable drive is unmounted); but
	// conversion has "solidify a snapshot + delete the source" semantics, and
	// if it reused that filter, converting during exactly the window an
	// external drive is unplugged would permanently lose the offline members
	// (the source smart_view is already deleted, with nowhere left to recover
	// them from). This only keeps the trash filter consistent with
	// MatchedAssets (deleted_at IS NULL, which makes sense — a soft-deleted
	// asset shouldn't be solidified) and the excluded-row filter (origin<>2),
	// dropping the offline filter, sorted by score DESC when writing.
	rows, err := s.db.Query(`
		SELECT m.asset_id FROM smart_view_matches m JOIN assets a ON a.id=m.asset_id
		WHERE m.smart_view_id=? AND m.origin<>2 AND a.deleted_at IS NULL
		ORDER BY m.match_score DESC`, id)
	if err != nil {
		compensateDeleteAlbum(albumSvc, album.ID, "query matched members failed")
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			compensateDeleteAlbum(albumSvc, album.ID, "scan matched members failed")
			return nil, err
		}
		ids = append(ids, aid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		compensateDeleteAlbum(albumSvc, album.ID, "iterate matched members failed")
		return nil, err
	}
	if len(ids) > 0 {
		if err := albumSvc.BatchAddAssets(album.ID, ids); err != nil {
			compensateDeleteAlbum(albumSvc, album.ID, "BatchAddAssets failed")
			return nil, err
		}
	}
	// No activity is logged: smart_view_activity is ON DELETE CASCADE against
	// smart_views, so the Delete right below would cascade away the log entry
	// we just wrote (dead code found during review), and once the sv is
	// deleted there's nowhere left to view its activity stream anyway.

	// The new identity is ready; delete the original smart view (cascading matches).
	if err := s.Delete(id); err != nil {
		compensateDeleteAlbum(albumSvc, album.ID, "delete original smart view failed")
		return nil, err
	}

	return albumSvc.Get(album.ID)
}

// ExportAsAlbum creates a new album snapshot from the smart view's current matches.
// Returns the new album's ID.
func (s *SmartViewService) ExportAsAlbum(id string) (string, error) {
	sv, err := s.Get(id)
	if err != nil {
		return "", err
	}
	albumSvc := NewAlbumService(s.db)
	album, err := albumSvc.Create(sv.Name + " (snapshot)")
	if err != nil {
		return "", fmt.Errorf("ExportAsAlbum create album: %w", err)
	}
	assets, err := s.MatchedAssets(id, 100000, 0, false, "")
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		ids = append(ids, a.ID)
	}
	if len(ids) > 0 {
		if err := albumSvc.BatchAddAssets(album.ID, ids); err != nil {
			return "", err
		}
	}
	s.logActivity(id, "exported", "album", nil)
	return album.ID, nil
}

// ExportZip streams all matched assets as a ZIP archive directly to w.
func (s *SmartViewService) ExportZip(w http.ResponseWriter, id string) error {
	assets, err := s.MatchedAssets(id, 100000, 0, false, "")
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		return ErrInvalidInput
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="smartview-%s.zip"`, id))
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, a := range assets {
		name := a.OriginalName
		if name == "" {
			name = a.ID + filepath.Ext(a.FilePath)
		}
		zf, err := zw.Create(name)
		if err != nil {
			continue
		}
		f, err := os.Open(a.FilePath)
		if err != nil {
			continue
		}
		_, _ = io.Copy(zf, f)
		f.Close()
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	s.logActivity(id, "exported", "zip", nil)
	return nil
}
