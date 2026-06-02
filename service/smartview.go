package service

import (
	"database/sql"
	"encoding/json"
	"errors"
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
	NotifyWeekly  bool       `json:"notifyWeekly"`
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
	NotifyWeekly  bool     `json:"notifyWeekly"`
}

type SmartViewPatch struct {
	Name          *string   `json:"name,omitempty"`
	Description   *string   `json:"description,omitempty"`
	CondsRaw      *[]string `json:"condsRaw,omitempty"`
	Threshold     *int      `json:"threshold,omitempty"`
	Live          *bool     `json:"live,omitempty"`
	IncludeVideos *bool     `json:"includeVideos,omitempty"`
	NotifyWeekly  *bool     `json:"notifyWeekly,omitempty"`
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
	parsed := ParseConditions(s.db, in.CondsRaw)
	parsedJSON, _ := json.Marshal(parsed)
	_, err := s.db.Exec(`INSERT INTO smart_views
		(id,name,description,conds_raw,conds_parsed,threshold,live,include_videos,notify_weekly)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		in.ID, in.Name, in.Description, string(rawJSON), string(parsedJSON),
		in.Threshold, boolToInt(in.Live), boolToInt(in.IncludeVideos), boolToInt(in.NotifyWeekly))
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
		sv                SmartView
		rawJSON           string
		liveI, vidI, notI int
		evaluatedAt       sql.NullTime
		desc              sql.NullString
	)
	err := s.db.QueryRow(`SELECT id,name,description,conds_raw,threshold,live,include_videos,notify_weekly,evaluated_at
		FROM smart_views WHERE id=?`, id).Scan(
		&sv.ID, &sv.Name, &desc, &rawJSON, &sv.Threshold, &liveI, &vidI, &notI, &evaluatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sv.Description = desc.String
	sv.Live = liveI == 1
	sv.IncludeVideos = vidI == 1
	sv.NotifyWeekly = notI == 1
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
	if p.NotifyWeekly != nil {
		sets = append(sets, "notify_weekly=?")
		args = append(args, boolToInt(*p.NotifyWeekly))
	}
	if p.CondsRaw != nil {
		rawJSON, _ := json.Marshal(*p.CondsRaw)
		parsed := ParseConditions(s.db, *p.CondsRaw)
		parsedJSON, _ := json.Marshal(parsed)
		sets = append(sets, "conds_raw=?", "conds_parsed=?")
		args = append(args, string(rawJSON), string(parsedJSON))
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
	if p.CondsRaw != nil || p.Threshold != nil || p.IncludeVideos != nil {
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
	sv.Distribution = []int{}
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
func (s *SmartViewService) Evaluate(id string) error {
	sv, err := s.Get(id)
	if err != nil {
		return err
	}
	var parsedJSON string
	var includeVideos int
	if err := s.db.QueryRow(`SELECT conds_parsed, include_videos FROM smart_views WHERE id=?`, id).
		Scan(&parsedJSON, &includeVideos); err != nil {
		return err
	}
	var parsed []ParsedCond
	_ = json.Unmarshal([]byte(parsedJSON), &parsed)

	var sets []map[string]struct{}
	semanticScores := map[string][]float64{}
	hasExecutable := false
	for _, c := range parsed {
		switch c.Kind {
		case condPerson:
			ids, err := s.assetsForPerson(c.Value)
			if err != nil {
				return err
			}
			sets = append(sets, ids)
			hasExecutable = true
		case condPlace:
			ids, err := s.assetsForPlace(c.Value)
			if err != nil {
				return err
			}
			sets = append(sets, ids)
			hasExecutable = true
		case condDate:
			if c.Start != nil && c.End != nil {
				ids, err := s.assetsForDateRange(*c.Start, *c.End)
				if err != nil {
					return err
				}
				sets = append(sets, ids)
				hasExecutable = true
			}
		case condSemantic:
			scored, err := s.assetsForSemantic(c.Value)
			if err != nil {
				return err
			}
			set := map[string]struct{}{}
			for aid, sc := range scored {
				set[aid] = struct{}{}
				semanticScores[aid] = append(semanticScores[aid], sc)
			}
			sets = append(sets, set)
			hasExecutable = true
		case condUnsupported:
		}
	}
	if !hasExecutable {
		_, _ = s.db.Exec(`DELETE FROM smart_view_matches WHERE smart_view_id=?`, id)
		s.touchEvaluated(id)
		return nil
	}
	inter := intersect(sets)
	if includeVideos == 0 {
		inter = s.excludeVideos(inter)
	}
	type scored struct {
		id    string
		score float64
	}
	thr := float64(sv.Threshold) / 100.0
	var keep []scored
	for aid := range inter {
		score := 1.0
		if vs := semanticScores[aid]; len(vs) > 0 {
			sum := 0.0
			for _, v := range vs {
				sum += v
			}
			score = sum / float64(len(vs))
		}
		if score >= thr {
			keep = append(keep, scored{aid, score})
		}
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
		WHERE fp.person_id=? AND a.is_live_photo_video=0 AND a.deleted_at IS NULL`, personID)
	return scanIDSet(rows, err)
}

func (s *SmartViewService) assetsForPlace(text string) (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT DISTINCT g.asset_id FROM asset_geo g
		JOIN assets a ON a.id=g.asset_id
		WHERE (lower(g.city)=lower(?) OR lower(g.country)=lower(?) OR lower(g.region)=lower(?))
		  AND a.is_live_photo_video=0 AND a.deleted_at IS NULL`, text, text, text)
	return scanIDSet(rows, err)
}

func (s *SmartViewService) assetsForDateRange(start, end time.Time) (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT id FROM assets
		WHERE taken_at BETWEEN ? AND ? AND is_live_photo_video=0 AND deleted_at IS NULL`,
		start.UTC().Format("2006-01-02T15:04:05Z"), end.UTC().Format("2006-01-02T15:04:05Z"))
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
	out := map[string]struct{}{}
	for aid := range in {
		var mime string
		s.db.QueryRow(`SELECT COALESCE(mime_type,'') FROM assets WHERE id=?`, aid).Scan(&mime)
		if len(mime) >= 6 && mime[:6] == "video/" {
			continue
		}
		out[aid] = struct{}{}
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
