package service

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/disintegration/imaging"
)

// PersonService provides People list/detail/edit/relations/places/merge suggestions.
type PersonService struct {
	db *sql.DB
}

func NewPersonService(db *sql.DB) *PersonService { return &PersonService{db: db} }

// ListPersons returns all non-hidden persons as rich objects (with count/confidence/first-last-seen/places count).
func (s *PersonService) ListPersons() ([]Person, error) {
	rows, err := s.db.Query(`
SELECT p.id, p.name, COALESCE(p.cover_asset_id,''), COALESCE(p.cover_face_id,''),
       p.favorite, COALESCE(p.relation,''), p.confidence,
       (SELECT COUNT(DISTINCT a.id)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.is_live_photo_video=0) AS cnt,
       (SELECT MIN(a.taken_at)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.is_live_photo_video=0) AS first_seen,
       (SELECT MAX(a.taken_at)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.is_live_photo_video=0) AS last_seen,
       (SELECT COUNT(DISTINCT (CAST(e.latitude*2 AS INT) || ',' || CAST(e.longitude*2 AS INT)))
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          JOIN asset_exif e ON e.asset_id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
            AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
            AND NOT (e.latitude=0 AND e.longitude=0)) AS places
FROM persons p
WHERE p.hidden=0
ORDER BY cnt DESC, p.rowid`)
	if err != nil {
		return nil, fmt.Errorf("ListPersons: %w", err)
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		var fav int
		var first, last sql.NullTime
		var places int
		if err := rows.Scan(&p.ID, &p.Name, &p.CoverAssetID, &p.CoverFaceID, &fav, &p.Relation, &p.Confidence, &p.Count, &first, &last, &places); err != nil {
			return nil, err
		}
		p.Favorite = fav != 0
		if first.Valid {
			tt := first.Time
			p.FirstSeen = &tt
		}
		if last.Valid {
			tt := last.Time
			p.LastSeen = &tt
		}
		p.PlacesCount = places
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPerson 返回单个 person 富对象（含 count/首末次出现/地点数）。
func (s *PersonService) GetPerson(id string) (*Person, error) {
	var p Person
	var fav int
	err := s.db.QueryRow(`
SELECT p.id, p.name, COALESCE(p.cover_asset_id,''), COALESCE(p.cover_face_id,''),
       p.favorite, COALESCE(p.relation,''), p.confidence
FROM persons p WHERE p.id=? AND p.hidden=0`, id).Scan(
		&p.ID, &p.Name, &p.CoverAssetID, &p.CoverFaceID, &fav, &p.Relation, &p.Confidence)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetPerson: %w", err)
	}
	p.Favorite = fav != 0

	// count / first / last
	var cnt int
	var first, last sql.NullTime
	if err := s.db.QueryRow(`
SELECT COUNT(DISTINCT a.id), MIN(a.taken_at), MAX(a.taken_at)
FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id
WHERE fp.person_id=? AND a.deleted_at IS NULL AND a.is_live_photo_video=0`, id).
		Scan(&cnt, &first, &last); err != nil {
		return nil, fmt.Errorf("GetPerson stats: %w", err)
	}
	p.Count = cnt
	if first.Valid {
		t := first.Time
		p.FirstSeen = &t
	}
	if last.Valid {
		t := last.Time
		p.LastSeen = &t
	}

	// placesCount：distinct 粗粒度 GPS cell（0.5° ≈ 城市级聚合），过滤 0,0 与软删/live video。
	var places int
	if err := s.db.QueryRow(`
SELECT COUNT(DISTINCT (CAST(e.latitude*2 AS INT) || ',' || CAST(e.longitude*2 AS INT)))
FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id
JOIN asset_exif e ON e.asset_id=fd.asset_id
WHERE fp.person_id=? AND a.deleted_at IS NULL AND a.is_live_photo_video=0
  AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
  AND NOT (e.latitude=0 AND e.longitude=0)`, id).Scan(&places); err != nil {
		return nil, fmt.Errorf("GetPerson places: %w", err)
	}
	p.PlacesCount = places
	return &p, nil
}

// PersonPatch 局部更新字段，nil 表示不改。
type PersonPatch struct {
	Name     *string
	Favorite *bool
	Relation *string
}

// UpdatePerson 局部更新 name/favorite/relation。person 不存在返回 ErrNotFound。
func (s *PersonService) UpdatePerson(id string, patch PersonPatch) error {
	sets := []string{}
	args := []any{}
	if patch.Name != nil {
		sets = append(sets, "name=?")
		args = append(args, *patch.Name)
	}
	if patch.Favorite != nil {
		v := 0
		if *patch.Favorite {
			v = 1
		}
		sets = append(sets, "favorite=?")
		args = append(args, v)
	}
	if patch.Relation != nil {
		sets = append(sets, "relation=?")
		args = append(args, *patch.Relation)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=CURRENT_TIMESTAMP")
	args = append(args, id)
	res, err := s.db.Exec(`UPDATE persons SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return fmt.Errorf("UpdatePerson: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HidePerson 软删除（hidden=1）。
func (s *PersonService) HidePerson(id string) error { return s.setHidden(id, true) }

// RestorePerson 恢复（hidden=0）。
func (s *PersonService) RestorePerson(id string) error { return s.setHidden(id, false) }

func (s *PersonService) setHidden(id string, hidden bool) error {
	v := 0
	if hidden {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE persons SET hidden=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, v, id)
	if err != nil {
		return fmt.Errorf("setHidden: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PersonRelations 返回与该 person 在同一 asset 共同出镜的其他 person，按共现次数降序。
func (s *PersonService) PersonRelations(id string) ([]PersonRelation, error) {
	rows, err := s.db.Query(`
SELECT fp2.person_id, COALESCE(p.name,''), COALESCE(p.cover_face_id,''), COUNT(DISTINCT a.id) AS cnt
FROM face_person fp1
JOIN face_detections fd1 ON fd1.id=fp1.face_id
JOIN face_detections fd2 ON fd2.asset_id=fd1.asset_id AND fd2.id!=fd1.id
JOIN face_person fp2 ON fp2.face_id=fd2.id
JOIN assets a ON a.id=fd1.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
JOIN persons p ON p.id=fp2.person_id AND p.hidden=0
WHERE fp1.person_id=? AND fp2.person_id!=fp1.person_id
GROUP BY fp2.person_id
ORDER BY cnt DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("PersonRelations: %w", err)
	}
	defer rows.Close()
	var out []PersonRelation
	for rows.Next() {
		var r PersonRelation
		if err := rows.Scan(&r.PersonID, &r.Name, &r.CoverFaceID, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FacesIndexedUpTo returns the most recent indexed_at of assets with face detections (for banner use), nil if none.
func (s *PersonService) FacesIndexedUpTo() (*string, error) {
	var ts sql.NullString
	err := s.db.QueryRow(`
SELECT MAX(a.indexed_at) FROM face_detections fd JOIN assets a ON a.id=fd.asset_id`).Scan(&ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !ts.Valid {
		return nil, nil
	}
	v := ts.String
	return &v, nil
}

// PersonPlaces 返回该 person 照片的 GPS 点（前端做国家/城市级聚合）。
func (s *PersonService) PersonPlaces(id string) ([]PersonPlace, error) {
	rows, err := s.db.Query(`
SELECT e.latitude, e.longitude, a.taken_at
FROM face_person fp
JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
JOIN asset_exif e ON e.asset_id=a.id
WHERE fp.person_id=? AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
  AND NOT (e.latitude=0 AND e.longitude=0)`, id)
	if err != nil {
		return nil, fmt.Errorf("PersonPlaces: %w", err)
	}
	defer rows.Close()
	var out []PersonPlace
	for rows.Next() {
		var pl PersonPlace
		var taken sql.NullTime
		if err := rows.Scan(&pl.Latitude, &pl.Longitude, &taken); err != nil {
			return nil, fmt.Errorf("PersonPlaces scan: %w", err)
		}
		if taken.Valid {
			tt := taken.Time
			pl.TakenAt = &tt
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

type personCentroid struct {
	id       string
	name     string
	faceID   string
	centroid []float32
}

// MergeSuggestions 返回质心距离落在 (dbscanEpsilon, suggestEpsilon) 带内、
// 且未被拒绝的 person 配对，confidence=1-dist，按 confidence 降序。
func (s *PersonService) MergeSuggestions() ([]MergeSuggestion, error) {
	rows, err := s.db.Query(`
SELECT id, COALESCE(name,''), COALESCE(cover_face_id,''), centroid
FROM persons WHERE hidden=0 AND centroid IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("MergeSuggestions load: %w", err)
	}
	var ps []personCentroid
	for rows.Next() {
		var pc personCentroid
		var blob []byte
		if err := rows.Scan(&pc.id, &pc.name, &pc.faceID, &blob); err != nil {
			rows.Close()
			return nil, fmt.Errorf("MergeSuggestions scan: %w", err)
		}
		pc.centroid = sqlite.DeserializeFloat32(blob)
		if len(pc.centroid) > 0 {
			ps = append(ps, pc)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("MergeSuggestions iter: %w", err)
	}

	rejected, err := s.loadRejections()
	if err != nil {
		return nil, err
	}

	var out []MergeSuggestion
	for i := 0; i < len(ps); i++ {
		for j := i + 1; j < len(ps); j++ {
			d := cosDist(ps[i].centroid, ps[j].centroid)
			if d <= dbscanEpsilon || d >= suggestEpsilon {
				continue
			}
			a, b := ps[i], ps[j]
			if rejected[pairKey(a.id, b.id)] {
				continue
			}
			// 目标取有名的一方；都无名/都有名则按 id 稳定。
			from, into := a, b
			switch {
			case a.name != "" && b.name == "":
				from, into = b, a // a 有名 → 取 a 为 into
			case a.name == "" && b.name == "" && a.id > b.id:
				from, into = b, a // 都无名 → 按 id 稳定
			case a.name != "" && b.name != "" && a.id > b.id:
				from, into = b, a // 都有名 → 按 id 稳定
			}
			conf := 1.0 - d
			out = append(out, MergeSuggestion{
				ID:         pairKey(a.id, b.id),
				FromID:     from.id,
				IntoID:     into.id,
				FromFaceID: from.faceID,
				IntoFaceID: into.faceID,
				IntoName:   into.name,
				Confidence: conf,
				Reason:     suggestionReason(into.name, conf),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	return out, nil
}

// RejectMerge 记住被拒绝的配对（方向无关）。
func (s *PersonService) RejectMerge(a, b string) error {
	pa, pb := orderPair(a, b)
	_, err := s.db.Exec(`INSERT OR IGNORE INTO merge_rejections(person_a, person_b) VALUES(?,?)`, pa, pb)
	if err != nil {
		return fmt.Errorf("RejectMerge: %w", err)
	}
	return nil
}

func (s *PersonService) loadRejections() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT person_a, person_b FROM merge_rejections`)
	if err != nil {
		return nil, fmt.Errorf("loadRejections: %w", err)
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("loadRejections scan: %w", err)
		}
		m[pairKey(a, b)] = true
	}
	return m, rows.Err()
}

func orderPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}
func pairKey(a, b string) string {
	x, y := orderPair(a, b)
	return x + "|" + y
}

func suggestionReason(name string, conf float64) string {
	if name != "" {
		return fmt.Sprintf("两个集群的人脸高度相似（%.0f%%），可能都是 %s。", conf*100, name)
	}
	return fmt.Sprintf("两个集群的人脸高度相似（%.0f%%），可能是同一个人。", conf*100)
}

// FaceThumbnail 按 cover_face 的 bbox 从原图裁出方形人脸缓存到 cacheDir，返回文件路径。
// 已缓存则直接返回。person 无 cover_face 或不存在返回 ErrNotFound。
func (s *PersonService) FaceThumbnail(personID, cacheDir string) (string, error) {
	var faceID, bbox, srcPath string
	err := s.db.QueryRow(`
SELECT fd.id, fd.bbox, a.file_path
FROM persons p
JOIN face_detections fd ON fd.id=p.cover_face_id
JOIN assets a ON a.id=fd.asset_id
WHERE p.id=? AND p.hidden=0`, personID).Scan(&faceID, &bbox, &srcPath)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("FaceThumbnail query: %w", err)
	}

	outPath := filepath.Join(cacheDir, faceID+".jpg")
	if st, statErr := os.Stat(outPath); statErr == nil && st.Size() > 0 {
		return outPath, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("FaceThumbnail mkdir: %w", err)
	}

	var bb struct {
		X1 float64 `json:"x1"`
		Y1 float64 `json:"y1"`
		X2 float64 `json:"x2"`
		Y2 float64 `json:"y2"`
	}
	if err := json.Unmarshal([]byte(bbox), &bb); err != nil {
		return "", fmt.Errorf("FaceThumbnail bbox: %w", err)
	}
	img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
	if err != nil {
		return "", fmt.Errorf("FaceThumbnail open: %w", err)
	}
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	// 归一化 → 像素，加 30% padding 取方形。
	cx := (bb.X1 + bb.X2) / 2 * float64(w)
	cy := (bb.Y1 + bb.Y2) / 2 * float64(h)
	side := (bb.X2 - bb.X1) * float64(w)
	if hSide := (bb.Y2 - bb.Y1) * float64(h); hSide > side {
		side = hSide
	}
	side *= 1.3
	half := side / 2
	x0 := int(cx - half)
	y0 := int(cy - half)
	x1 := int(cx + half)
	y1 := int(cy + half)
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > w {
		x1 = w
	}
	if y1 > h {
		y1 = h
	}
	cropped := imaging.Crop(img, image.Rect(x0, y0, x1, y1))
	square := imaging.Fill(cropped, 256, 256, imaging.Center, imaging.Lanczos)
	if err := imaging.Save(square, outPath); err != nil {
		return "", fmt.Errorf("FaceThumbnail save: %w", err)
	}
	return outPath, nil
}
