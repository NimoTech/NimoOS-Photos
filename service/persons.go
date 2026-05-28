package service

import (
	"database/sql"
	"fmt"
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
