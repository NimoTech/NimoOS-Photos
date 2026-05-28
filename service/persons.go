package service

import (
	"database/sql"
	"fmt"
	"strings"
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
