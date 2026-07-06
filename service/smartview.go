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
)

type SmartView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
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
	if in.ID == "" || in.Name == "" {
		return nil, ErrInvalidInput
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
	if err != nil {
		return nil, err
	}
	s.logActivity(in.ID, "created", "", nil)
	if err := s.Evaluate(in.ID); err != nil {
		return nil, err
	}
	return s.Get(in.ID)
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
	err := s.db.QueryRow(`SELECT id,name,description,conds_raw,threshold,live,include_videos,evaluated_at
		FROM smart_views WHERE id=?`, id).Scan(
		&sv.ID, &sv.Name, &desc, &rawJSON, &sv.Threshold, &liveI, &vidI, &evaluatedAt)
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
	// 恢复 live(取消暂停)也要重算:暂停期间 displayScore 标定端点
	// (simDisplayFloor/Ceil)可能已随模型换代调整,旧 match_score 是旧标度。
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

	_ = s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id=?`, sv.ID).Scan(&sv.Count)

	_ = s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches
		WHERE smart_view_id=? AND matched_at >= datetime('now','-7 days')`, sv.ID).Scan(&sv.AddedThisWeek)

	_ = s.db.QueryRow(`SELECT COALESCE(SUM(a.file_size),0) FROM smart_view_matches m
		JOIN assets a ON a.id=m.asset_id WHERE m.smart_view_id=?`, sv.ID).Scan(&sv.StorageBytes)

	rows, err := s.db.Query(`SELECT match_score FROM smart_view_matches WHERE smart_view_id=? ORDER BY match_score`, sv.ID)
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

	srows, err := s.db.Query(`SELECT asset_id FROM smart_view_matches
		WHERE smart_view_id=? ORDER BY match_score DESC LIMIT 6`, sv.ID)
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
	where := `m.smart_view_id=?`
	args := []any{userID, id}
	if recent {
		where += ` AND m.matched_at >= datetime('now','-7 days')`
	}
	// julianday() tolerates both "YYYY-MM-DD HH:MM:SS" (CURRENT_TIMESTAMP) and
	// ISO "T...Z" / "+00:00" (driver-written time.Time) datetime encodings.
	q := `SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
	       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
	       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8),
	       a.indexed_at, a.status, m.match_score,
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
		// duration_ms / taken_at / file_size / indexed_at are nullable in the
		// assets table — scan through sql.Null* like scanAssets does, or a single
		// NULL (every photo's duration_ms) fails the whole query.
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		if err := rows.Scan(&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
			&indexedAt, &a.Status, &score, &a.IsNew); err != nil {
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
		if indexedAt.Valid {
			t := indexedAt.Time
			a.IndexedAt = &t
		}
		a.MatchScore = &score
		out = append(out, a)
	}
	return out, nil
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
	existing := map[string]struct{}{}
	rows, err := s.db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id=?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var aid string
		rows.Scan(&aid)
		existing[aid] = struct{}{}
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
		if _, ok := existing[k.id]; ok {
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
	for aid := range existing {
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
	results, err := s.search.SmartSearch(query, 500, SearchFilters{})
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
