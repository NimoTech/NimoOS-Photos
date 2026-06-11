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
	"time"

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
		var first, last sql.NullString
		var places int
		if err := rows.Scan(&p.ID, &p.Name, &p.CoverAssetID, &p.CoverFaceID, &fav, &p.Relation, &p.Confidence, &p.Count, &first, &last, &places); err != nil {
			return nil, err
		}
		p.Favorite = fav != 0
		if tt := parseSQLiteTime(first); tt != nil {
			p.FirstSeen = tt
		}
		if tt := parseSQLiteTime(last); tt != nil {
			p.LastSeen = tt
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
	var first, last sql.NullString
	if err := s.db.QueryRow(`
SELECT COUNT(DISTINCT a.id), MIN(a.taken_at), MAX(a.taken_at)
FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id
WHERE fp.person_id=? AND a.deleted_at IS NULL AND a.is_live_photo_video=0`, id).
		Scan(&cnt, &first, &last); err != nil {
		return nil, fmt.Errorf("GetPerson stats: %w", err)
	}
	p.Count = cnt
	if tt := parseSQLiteTime(first); tt != nil {
		p.FirstSeen = tt
	}
	if tt := parseSQLiteTime(last); tt != nil {
		p.LastSeen = tt
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

// PurgePerson permanently deletes a person group in a single transaction:
//  1. Mark all face_detections that belonged to the person as excluded=1 so they
//     never participate in future clustering or attachment.
//  2. Remove all face_person bindings for the person.
//  3. Delete the persons row itself.
//
// Assets are never touched. The operation is intentionally unrestricted —
// anchored (named/favorited) persons can be purged because this is an explicit
// user action. Returns ErrNotFound if no person with the given id exists.
func (s *PersonService) PurgePerson(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("PurgePerson begin: %w", err)
	}
	defer tx.Rollback()

	// Verify the person exists (mirrors HidePerson's not-found semantics via
	// setHidden which checks RowsAffected; here we pre-check with a SELECT so
	// all three subsequent statements can assume existence).
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("PurgePerson check: %w", err)
	}

	// Step 1: mark all face_detections that belong to this person as excluded=1.
	if _, err := tx.Exec(`
		UPDATE face_detections SET excluded=1
		WHERE id IN (SELECT face_id FROM face_person WHERE person_id=?)`, id); err != nil {
		return fmt.Errorf("PurgePerson exclude faces: %w", err)
	}

	// Step 2: delete face_person bindings.
	if _, err := tx.Exec(`DELETE FROM face_person WHERE person_id=?`, id); err != nil {
		return fmt.Errorf("PurgePerson delete face_person: %w", err)
	}

	// Step 3: delete the person row.
	if _, err := tx.Exec(`DELETE FROM persons WHERE id=?`, id); err != nil {
		return fmt.Errorf("PurgePerson delete person: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("PurgePerson commit: %w", err)
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
		var taken sql.NullString
		if err := rows.Scan(&pl.Latitude, &pl.Longitude, &taken); err != nil {
			return nil, fmt.Errorf("PersonPlaces scan: %w", err)
		}
		if tt := parseSQLiteTime(taken); tt != nil {
			pl.TakenAt = tt
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

// parseSQLiteTime parses a TEXT timestamp returned by SQLite (from a direct
// column or an aggregate like MIN/MAX) using the formats GORM writes:
// RFC3339 with offset, or the legacy "2006-01-02 15:04:05.000000-07:00" form.
// Returns nil if the string is NULL/empty or unparseable.
func parseSQLiteTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s.String); err == nil {
			return &t
		}
	}
	return nil
}

// DetachAssetsFromPerson 把若干 asset 中所有属于该 person 的脸从该 person 移除，
// 同时把这些脸标记为 excluded=1（不再参与未来聚类/吸附/列表）。
// 自动 person（非锚定）若移除后剩 0 脸则连带删除。
//
// 返回值 affected 为本次实际移除的 face 数。如果 person 不存在返回 ErrNotFound。
func (s *PersonService) DetachAssetsFromPerson(personID string, assetIDs []string) (int, error) {
	if personID == "" {
		return 0, ErrNotFound
	}
	if len(assetIDs) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson begin: %w", err)
	}
	defer tx.Rollback()

	// 校验 person 存在
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, personID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("DetachAssetsFromPerson check: %w", err)
	}

	// 找出该 person 在指定 asset 中已绑定且未被 excluded 的脸
	placeholders := make([]string, len(assetIDs))
	args := make([]any, 0, len(assetIDs)+1)
	args = append(args, personID)
	for i, id := range assetIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := fmt.Sprintf(`
SELECT fd.id
FROM face_person fp
JOIN face_detections fd ON fd.id = fp.face_id
WHERE fp.person_id = ? AND fd.excluded = 0 AND fd.asset_id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := tx.Query(q, args...)
	if err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson select: %w", err)
	}
	var faceIDs []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			rows.Close()
			return 0, err
		}
		faceIDs = append(faceIDs, fid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(faceIDs) == 0 {
		return 0, nil
	}

	// 标 excluded + 解绑
	fph := make([]string, len(faceIDs))
	fargs := make([]any, len(faceIDs))
	for i, fid := range faceIDs {
		fph[i] = "?"
		fargs[i] = fid
	}
	if _, err := tx.Exec(
		fmt.Sprintf(`UPDATE face_detections SET excluded=1 WHERE id IN (%s)`, strings.Join(fph, ",")),
		fargs...); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson mark excluded: %w", err)
	}
	if _, err := tx.Exec(
		fmt.Sprintf(`DELETE FROM face_person WHERE face_id IN (%s)`, strings.Join(fph, ",")),
		fargs...); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson unbind: %w", err)
	}

	// 重算质心 / cover / confidence（vecs=0 时 recomputeOneCentroidTx 直接 return，不会清空字段）
	if err := recomputeOneCentroidTx(tx, personID); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson recompute: %w", err)
	}

	// 若剩余 0 脸且 person 非锚定，删除该 person
	var remaining int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, personID).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson count: %w", err)
	}
	if remaining == 0 {
		var name, relation string
		var fav, hidden int
		if err := tx.QueryRow(
			`SELECT COALESCE(name,''), favorite, COALESCE(relation,''), hidden FROM persons WHERE id=?`, personID,
		).Scan(&name, &fav, &relation, &hidden); err != nil {
			return 0, fmt.Errorf("DetachAssetsFromPerson load anchor: %w", err)
		}
		isAnchored := name != "" || fav != 0 || relation != "" || hidden != 0
		if !isAnchored {
			if _, err := tx.Exec(`DELETE FROM persons WHERE id=?`, personID); err != nil {
				return 0, fmt.Errorf("DetachAssetsFromPerson delete empty: %w", err)
			}
		} else {
			// 锚定 person 保留，但清空 cover_*（recomputeOneCentroidTx 在 vecs=0 时已 return，未更新）
			if _, err := tx.Exec(
				`UPDATE persons SET cover_face_id=NULL, cover_asset_id=NULL, centroid=NULL, confidence=0, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
				personID,
			); err != nil {
				return 0, fmt.Errorf("DetachAssetsFromPerson clear cover: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson commit: %w", err)
	}
	return len(faceIDs), nil
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

// recomputeOneCentroidTx 在事务内重算单个 person 的 centroid/confidence/cover_face_id。
func recomputeOneCentroidTx(tx *sql.Tx, personID string) error {
	rows, err := tx.Query(`
SELECT fd.id, fd.asset_id, fd.embedding
FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
WHERE fp.person_id=?`, personID)
	if err != nil {
		return err
	}
	var faceIDs, assetIDs []string
	var vecs [][]float32
	for rows.Next() {
		var fid, aid string
		var blob []byte
		if err := rows.Scan(&fid, &aid, &blob); err != nil {
			rows.Close()
			return err
		}
		faceIDs = append(faceIDs, fid)
		assetIDs = append(assetIDs, aid)
		vecs = append(vecs, sqlite.DeserializeFloat32(blob))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(vecs) == 0 {
		return nil
	}
	centroid := ComputeCentroid(vecs)
	conf := ClusterConfidence(vecs, centroid)
	best, bestDist := 0, 2.0
	for i, v := range vecs {
		if d := cosDist(v, centroid); d < bestDist {
			bestDist = d
			best = i
		}
	}
	_, err = tx.Exec(`UPDATE persons SET centroid=?, confidence=?, cover_face_id=?, cover_asset_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		sqlite.SerializeFloat32(centroid), conf, faceIDs[best], assetIDs[best], personID)
	return err
}

// FaceThumbnail 按 cover_face 的 bbox 从原图裁出方形人脸缓存到 cacheDir，返回文件路径。
// 已缓存则直接返回。person 无 cover_face 或不存在返回 ErrNotFound。
// 视频源用已生成的 large.jpg 缩略图（face detection 时也是对关键帧做的，bbox 归一化后与
// 缩略图比例一致）；图片源仍用原始文件以保留清晰度。
func (s *PersonService) FaceThumbnail(personID, cacheDir, thumbDir string) (string, error) {
	var faceID, bbox, srcPath, mimeType, assetID string
	var origW, origH sql.NullInt64
	err := s.db.QueryRow(`
SELECT fd.id, fd.bbox, a.file_path, COALESCE(a.mime_type,''), a.id, e.width, e.height
FROM persons p
JOIN face_detections fd ON fd.id=p.cover_face_id
JOIN assets a ON a.id=fd.asset_id
LEFT JOIN asset_exif e ON e.asset_id=a.id
WHERE p.id=? AND p.hidden=0`, personID).Scan(&faceID, &bbox, &srcPath, &mimeType, &assetID, &origW, &origH)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("FaceThumbnail query: %w", err)
	}

	if strings.HasPrefix(mimeType, "video/") {
		srcPath = filepath.Join(thumbDir, assetID, "large.jpg")
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

	// bbox 是基于 ML 输入图（视频关键帧或原图）的绝对像素坐标。
	// 视频被压成 thumb large.jpg（max 长边 1280px）后，bbox 必须按 thumb/原 比例缩放。
	// 图片源就是原图，sx=sy=1（除非缺 EXIF W/H，此时回退到 1:1，bbox 当作就是当前图的像素）。
	sx, sy := 1.0, 1.0
	if origW.Valid && origH.Valid && origW.Int64 > 0 && origH.Int64 > 0 {
		sx = float64(w) / float64(origW.Int64)
		sy = float64(h) / float64(origH.Int64)
	}
	cx := (bb.X1 + bb.X2) / 2 * sx
	cy := (bb.Y1 + bb.Y2) / 2 * sy
	side := (bb.X2 - bb.X1) * sx
	if hSide := (bb.Y2 - bb.Y1) * sy; hSide > side {
		side = hSide
	}
	if side <= 0 {
		return "", fmt.Errorf("FaceThumbnail: degenerate bbox %+v", bb)
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
	if x1 <= x0 || y1 <= y0 {
		return "", fmt.Errorf("FaceThumbnail: empty crop rect after clamp")
	}
	cropped := imaging.Crop(img, image.Rect(x0, y0, x1, y1))
	square := imaging.Fill(cropped, 256, 256, imaging.Center, imaging.Lanczos)
	if err := imaging.Save(square, outPath); err != nil {
		return "", fmt.Errorf("FaceThumbnail save: %w", err)
	}
	return outPath, nil
}
