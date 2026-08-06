package service

import (
	"database/sql"
	"encoding/json"
	"errors"
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
SELECT p.id, p.name,
       COALESCE(
           (SELECT a.id FROM assets a WHERE a.id=p.cover_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           (SELECT a.id FROM assets a WHERE a.id=p.hero_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           '') AS cover,
       COALESCE(p.cover_face_id,''),
       p.favorite, COALESCE(p.relation,''), p.confidence,
       (SELECT COUNT(DISTINCT a.id)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0) AS cnt,
       (SELECT MIN(a.taken_at)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0) AS first_seen,
       (SELECT MAX(a.taken_at)
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0) AS last_seen,
       (SELECT COUNT(DISTINCT (CAST(e.latitude*2 AS INT) || ',' || CAST(e.longitude*2 AS INT)))
          FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
          JOIN assets a ON a.id=fd.asset_id
          JOIN asset_exif e ON e.asset_id=fd.asset_id
          WHERE fp.person_id=p.id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
            AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
            AND NOT (e.latitude=0 AND e.longitude=0)) AS places,
       COALESCE(
           (SELECT a.id FROM assets a WHERE a.id=p.hero_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           (SELECT a2.id FROM face_person fp2
               JOIN face_detections fd2 ON fd2.id=fp2.face_id AND fd2.excluded=0
               JOIN assets a2 ON a2.id=fd2.asset_id AND a2.deleted_at IS NULL AND a2.offline=0
               WHERE fp2.person_id=p.id AND a2.aesthetic_score IS NOT NULL
               ORDER BY a2.aesthetic_score DESC LIMIT 1),
           '') AS hero
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
		if err := rows.Scan(&p.ID, &p.Name, &p.CoverAssetID, &p.CoverFaceID, &fav, &p.Relation, &p.Confidence, &p.Count, &first, &last, &places, &p.HeroAssetID); err != nil {
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

// GetPerson returns a single rich person object (with count/first-last-seen/places count).
func (s *PersonService) GetPerson(id string) (*Person, error) {
	var p Person
	var fav int
	err := s.db.QueryRow(`
SELECT p.id, p.name,
       COALESCE(
           (SELECT a.id FROM assets a WHERE a.id=p.cover_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           (SELECT a.id FROM assets a WHERE a.id=p.hero_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           '') AS cover,
       COALESCE(p.cover_face_id,''),
       p.favorite, COALESCE(p.relation,''), p.confidence,
       COALESCE(
           (SELECT a.id FROM assets a WHERE a.id=p.hero_asset_id AND a.deleted_at IS NULL AND a.offline=0),
           (SELECT a2.id FROM face_person fp2
               JOIN face_detections fd2 ON fd2.id=fp2.face_id AND fd2.excluded=0
               JOIN assets a2 ON a2.id=fd2.asset_id AND a2.deleted_at IS NULL AND a2.offline=0
               WHERE fp2.person_id=p.id AND a2.aesthetic_score IS NOT NULL
               ORDER BY a2.aesthetic_score DESC LIMIT 1),
           '') AS hero
FROM persons p WHERE p.id=? AND p.hidden=0`, id).Scan(
		&p.ID, &p.Name, &p.CoverAssetID, &p.CoverFaceID, &fav, &p.Relation, &p.Confidence, &p.HeroAssetID)
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
WHERE fp.person_id=? AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0`, id).
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

	// placesCount: distinct coarse-grained GPS cell (0.5° ≈ city-level aggregation), filters out 0,0 and soft-deleted/live video.
	var places int
	if err := s.db.QueryRow(`
SELECT COUNT(DISTINCT (CAST(e.latitude*2 AS INT) || ',' || CAST(e.longitude*2 AS INT)))
FROM face_person fp JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id
JOIN asset_exif e ON e.asset_id=fd.asset_id
WHERE fp.person_id=? AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
  AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
  AND NOT (e.latitude=0 AND e.longitude=0)`, id).Scan(&places); err != nil {
		return nil, fmt.Errorf("GetPerson places: %w", err)
	}
	p.PlacesCount = places
	return &p, nil
}

// PersonPatch holds the fields that may be partially updated on a person.
// A nil pointer means "leave unchanged". HeroAssetID uses a pointer-to-string
// so that empty string can be used to clear the field (set to NULL).
type PersonPatch struct {
	Name     *string
	Favorite *bool
	Relation *string
	// HeroAssetID: non-nil means update; empty string clears the field.
	// The asset must contain at least one excluded=0 face for this person;
	// otherwise UpdatePerson returns ErrNotFound.
	HeroAssetID *string
}

// UpdatePerson partially updates name/favorite/relation/heroAssetId.
// Returns ErrNotFound when the person does not exist or heroAssetId validation fails.
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
	if patch.HeroAssetID != nil {
		heroVal := *patch.HeroAssetID
		if heroVal == "" {
			// Clear hero.
			sets = append(sets, "hero_asset_id=NULL")
		} else {
			// Validate: the asset must have at least one excluded=0 face for this person.
			var faceCount int
			if err := s.db.QueryRow(`
SELECT COUNT(*) FROM face_person fp
JOIN face_detections fd ON fd.id=fp.face_id
WHERE fp.person_id=? AND fd.asset_id=? AND fd.excluded=0`, id, heroVal).Scan(&faceCount); err != nil {
				return fmt.Errorf("UpdatePerson hero validate: %w", err)
			}
			if faceCount == 0 {
				return ErrNotFound
			}
			sets = append(sets, "hero_asset_id=?")
			args = append(args, heroVal)
		}
	}
	if len(sets) == 0 {
		// No-op: still verify the person exists.
		var exists int
		if err := s.db.QueryRow(`SELECT 1 FROM persons WHERE id=?`, id).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("UpdatePerson: %w", err)
		}
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

// SetPersonCover pins the cover face/asset for a person to the best face found
// on the given asset, and sets cover_locked=1. The "best" face is the one with
// the largest bounding-box area among the person's excluded=0 faces on that asset.
// Returns the new cover face ID. Returns ErrNotFound if the person does not exist
// or if no valid face for that person is found on the asset.
func (s *PersonService) SetPersonCover(personID, assetID string) (string, error) {
	// Load all excluded=0 faces for this person on the specified asset, ordered by bbox area DESC.
	rows, err := s.db.Query(`
SELECT fd.id, fd.bbox
FROM face_person fp
JOIN face_detections fd ON fd.id=fp.face_id
WHERE fp.person_id=? AND fd.asset_id=? AND fd.excluded=0`, personID, assetID)
	if err != nil {
		return "", fmt.Errorf("SetPersonCover query: %w", err)
	}
	defer rows.Close()

	type faceCandidate struct {
		id   string
		area float64
	}
	var candidates []faceCandidate
	for rows.Next() {
		var fid, bboxStr string
		if err := rows.Scan(&fid, &bboxStr); err != nil {
			return "", fmt.Errorf("SetPersonCover scan: %w", err)
		}
		var bb struct {
			X1 float64 `json:"x1"`
			Y1 float64 `json:"y1"`
			X2 float64 `json:"x2"`
			Y2 float64 `json:"y2"`
		}
		if jsonErr := json.Unmarshal([]byte(bboxStr), &bb); jsonErr == nil {
			area := (bb.X2 - bb.X1) * (bb.Y2 - bb.Y1)
			candidates = append(candidates, faceCandidate{id: fid, area: area})
		} else {
			// Unparseable bbox: still a candidate, area=0.
			candidates = append(candidates, faceCandidate{id: fid, area: 0})
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("SetPersonCover iter: %w", err)
	}

	if len(candidates) == 0 {
		return "", ErrNotFound
	}

	// Select the face with the largest bbox area.
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.area > best.area {
			best = c
		}
	}

	// Write cover_face_id, cover_asset_id, cover_locked=1.
	res, err := s.db.Exec(`
UPDATE persons SET cover_face_id=?, cover_asset_id=?, cover_locked=1, updated_at=CURRENT_TIMESTAMP
WHERE id=?`, best.id, assetID, personID)
	if err != nil {
		return "", fmt.Errorf("SetPersonCover update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return best.id, nil
}

// UnlockPersonCover clears cover_locked=0 and immediately recomputes the cover
// face/asset using the centroid-nearest-face algorithm (same as recomputeOneCentroidTx).
// Returns the new cover face ID after recomputation. Returns ErrNotFound if the
// person does not exist.
func (s *PersonService) UnlockPersonCover(personID string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("UnlockPersonCover begin: %w", err)
	}
	defer tx.Rollback()

	// Verify person exists.
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, personID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("UnlockPersonCover check: %w", err)
	}

	// Clear the lock.
	if _, err := tx.Exec(`UPDATE persons SET cover_locked=0, updated_at=CURRENT_TIMESTAMP WHERE id=?`, personID); err != nil {
		return "", fmt.Errorf("UnlockPersonCover unlock: %w", err)
	}

	// Recompute cover (now that lock=0, recomputeOneCentroidTx will update it).
	if err := recomputeOneCentroidTx(tx, personID); err != nil {
		return "", fmt.Errorf("UnlockPersonCover recompute: %w", err)
	}

	// Read back the new cover_face_id.
	var coverFaceID sql.NullString
	if err := tx.QueryRow(`SELECT cover_face_id FROM persons WHERE id=?`, personID).Scan(&coverFaceID); err != nil {
		return "", fmt.Errorf("UnlockPersonCover read: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("UnlockPersonCover commit: %w", err)
	}
	return coverFaceID.String, nil
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

// HidePerson soft-deletes the person (hidden=1).
func (s *PersonService) HidePerson(id string) error { return s.setHidden(id, true) }

// RestorePerson restores the person (hidden=0).
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

// PersonRelations returns other persons who co-appear with this person in the same asset, ordered by co-occurrence count descending.
func (s *PersonService) PersonRelations(id string) ([]PersonRelation, error) {
	rows, err := s.db.Query(`
SELECT fp2.person_id, COALESCE(p.name,''), COALESCE(p.cover_face_id,''), COUNT(DISTINCT a.id) AS cnt
FROM face_person fp1
JOIN face_detections fd1 ON fd1.id=fp1.face_id
JOIN face_detections fd2 ON fd2.asset_id=fd1.asset_id AND fd2.id!=fd1.id
JOIN face_person fp2 ON fp2.face_id=fd2.id
JOIN assets a ON a.id=fd1.asset_id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
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

// PersonPlaces returns the GPS points from this person's photos (the frontend does country/city-level aggregation).
// Each point is enriched with a human-readable PlaceName from asset_geo using
// the same rule as enrichPlaceNames: "City, Country" when both are available,
// "City" when only city is known, or empty string otherwise.
func (s *PersonService) PersonPlaces(id string) ([]PersonPlace, error) {
	rows, err := s.db.Query(`
SELECT e.latitude, e.longitude, a.taken_at,
       COALESCE(g.city, ''), COALESCE(g.country, '')
FROM face_person fp
JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
JOIN asset_exif e ON e.asset_id=a.id
LEFT JOIN asset_geo g ON g.asset_id=a.id
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
		var city, country string
		if err := rows.Scan(&pl.Latitude, &pl.Longitude, &taken, &city, &country); err != nil {
			return nil, fmt.Errorf("PersonPlaces scan: %w", err)
		}
		if tt := parseSQLiteTime(taken); tt != nil {
			pl.TakenAt = tt
		}
		// Assemble place name following the same rule as enrichPlaceNames:
		// "City, Country" > "City" > "".
		if city != "" && country != "" {
			pl.PlaceName = city + ", " + country
		} else if city != "" {
			pl.PlaceName = city
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

// MergeSuggestions returns person pairs whose centroid distance falls within
// the (dbscanEpsilon, suggestEpsilon) band and that haven't been rejected,
// with confidence=1-dist, ordered by confidence descending.
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
			// The target is the named side; if both are named or both are
			// unnamed, break ties by id for stability.
			from, into := a, b
			switch {
			case a.name != "" && b.name == "":
				from, into = b, a // a is named → take a as into
			case a.name == "" && b.name == "" && a.id > b.id:
				from, into = b, a // both unnamed → stable by id
			case a.name != "" && b.name != "" && a.id > b.id:
				from, into = b, a // both named → stable by id
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

// RejectMerge remembers a rejected pair (direction-independent).
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

// DetachAssetsFromPerson removes all faces belonging to this person across
// the given assets from that person, marking those faces as excluded=1 (so
// they no longer participate in future clustering/attachment/lists).
// An automatic (non-anchored) person is deleted along with its faces if 0
// faces remain after removal.
//
// The return value affected is the number of faces actually removed in this
// call. Returns ErrNotFound if the person doesn't exist.
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

	// Verify the person exists.
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM persons WHERE id=?`, personID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("DetachAssetsFromPerson check: %w", err)
	}

	// Find the faces of this person on the given assets that are still bound and not yet excluded.
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

	// Mark excluded + unbind.
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

	// Clear hero_asset_id when the hero asset is among the detached assets.
	assetSet := make(map[string]bool, len(assetIDs))
	for _, aid := range assetIDs {
		assetSet[aid] = true
	}
	var currentHero sql.NullString
	if err := tx.QueryRow(`SELECT hero_asset_id FROM persons WHERE id=?`, personID).Scan(&currentHero); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson load hero: %w", err)
	}
	if currentHero.Valid && currentHero.String != "" && assetSet[currentHero.String] {
		if _, err := tx.Exec(`UPDATE persons SET hero_asset_id=NULL WHERE id=?`, personID); err != nil {
			return 0, fmt.Errorf("DetachAssetsFromPerson clear hero: %w", err)
		}
	}

	// Recompute centroid / cover / confidence (recomputeOneCentroidTx clears
	// cover/centroid when vecs=0, i.e. all faces were just detached).
	if err := recomputeOneCentroidTx(tx, personID); err != nil {
		return 0, fmt.Errorf("DetachAssetsFromPerson recompute: %w", err)
	}

	// If 0 faces remain and person is not anchored, delete it.
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
		}
		// Anchored persons are kept; cover/centroid/lock/hero are already
		// cleared above by recomputeOneCentroidTx's vecs=0 branch.
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
		return fmt.Sprintf("Two clusters look %.0f%% alike — likely both %s.", conf*100, name)
	}
	return fmt.Sprintf("Two clusters look %.0f%% alike — likely the same person.", conf*100)
}

// recomputeOneCentroidTx recomputes centroid and confidence for a person within a
// transaction. Cover face/asset are only updated when cover_locked=0.
//
// Lock-invalidation rule: if cover_locked=1 but the currently stored cover_face_id
// is no longer in the person's excluded=0 face set (e.g. it was detached), the lock
// is cleared and cover is reselected by centroid distance.
func recomputeOneCentroidTx(tx *sql.Tx, personID string) error {
	// Load all active (excluded=0) faces for centroid computation, and at the
	// same time fetch the asset aesthetic score and EXIF width/height needed
	// for the hybrid cover score (JOIN assets for the score, LEFT JOIN
	// asset_exif for width/height — when missing, hybridCoverScore marks that
	// face as incomparable).
	rows, err := tx.Query(`
SELECT fd.id, fd.asset_id, fd.embedding, fd.bbox,
       a.aesthetic_score, e.width, e.height
FROM face_person fp
JOIN face_detections fd ON fd.id=fp.face_id
JOIN assets a ON a.id=fd.asset_id
LEFT JOIN asset_exif e ON e.asset_id=fd.asset_id
WHERE fp.person_id=? AND fd.excluded=0`, personID)
	if err != nil {
		return err
	}
	var faceIDs, assetIDs, bboxes []string
	var vecs [][]float32
	var scores []sql.NullFloat64
	var ws, hs []sql.NullInt64
	for rows.Next() {
		var fid, aid, bbox string
		var blob []byte
		var score sql.NullFloat64
		var w, h sql.NullInt64
		if err := rows.Scan(&fid, &aid, &blob, &bbox, &score, &w, &h); err != nil {
			rows.Close()
			return err
		}
		faceIDs = append(faceIDs, fid)
		assetIDs = append(assetIDs, aid)
		bboxes = append(bboxes, bbox)
		vecs = append(vecs, sqlite.DeserializeFloat32(blob))
		scores = append(scores, score)
		ws = append(ws, w)
		hs = append(hs, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(vecs) == 0 {
		// No active faces left: clear cover/centroid so the person stops
		// pointing at detached or deleted faces (dangling cover_face_id).
		_, err = tx.Exec(`UPDATE persons SET cover_face_id=NULL, cover_asset_id=NULL,
			cover_locked=0, hero_asset_id=NULL, centroid=NULL, confidence=0,
			updated_at=CURRENT_TIMESTAMP WHERE id=?`, personID)
		return err
	}
	centroid := ComputeCentroid(vecs)
	conf := ClusterConfidence(vecs, centroid)

	// Check whether cover is locked and whether the locked face is still valid.
	var locked int
	var lockedFaceID sql.NullString
	if err := tx.QueryRow(`SELECT cover_locked, cover_face_id FROM persons WHERE id=?`, personID).
		Scan(&locked, &lockedFaceID); err != nil {
		return err
	}

	if locked == 1 && lockedFaceID.Valid && lockedFaceID.String != "" {
		// Check if the locked face is still in the active face set.
		lockedFaceStillValid := false
		for _, fid := range faceIDs {
			if fid == lockedFaceID.String {
				lockedFaceStillValid = true
				break
			}
		}
		if lockedFaceStillValid {
			// Locked face is valid: keep cover, only update centroid/confidence.
			_, err = tx.Exec(
				`UPDATE persons SET centroid=?, confidence=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
				sqlite.SerializeFloat32(centroid), conf, personID)
			return err
		}
		// Locked face is gone: clear the lock and fall through to reselect.
		if _, err := tx.Exec(`UPDATE persons SET cover_locked=0 WHERE id=?`, personID); err != nil {
			return err
		}
	}
	// Hybrid-score selection: whole-image aesthetic score × face area ratio;
	// incomparable (no score / no EXIF / degenerate bbox) is recorded as -1.
	// When all faces are incomparable, fall back to nearest-centroid (the
	// original behavior, which also covers the transition period for
	// existing libraries that haven't been scored yet).
	best, bestHybrid := -1, -1.0
	for i := range vecs {
		if h := hybridCoverScore(scores[i], bboxes[i], ws[i], hs[i]); h > bestHybrid {
			bestHybrid = h
			best = i
		}
	}
	if bestHybrid < 0 {
		// best defaults to 0 as a fallback: in the theoretical edge case where
		// every face's cosDist exactly equals the initial bestDist (2.0) (the
		// strict less-than check never fires), the loop body never updates
		// best, so it must still land on a valid face index to avoid an
		// out-of-bounds panic on faceIDs[best] below.
		best = 0
		bestDist := 2.0
		for i, v := range vecs {
			if d := cosDist(v, centroid); d < bestDist {
				bestDist = d
				best = i
			}
		}
	}
	_, err = tx.Exec(
		`UPDATE persons SET centroid=?, confidence=?, cover_face_id=?, cover_asset_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		sqlite.SerializeFloat32(centroid), conf, faceIDs[best], assetIDs[best], personID)
	return err
}

// hybridCoverScore computes a person's cover hybrid score (whole-image
// aesthetic score × face area ratio); returns -1 when incomparable.
// Incomparable scenarios: the asset hasn't been scored, EXIF is missing
// width/height, or the bbox is unparseable or degenerate (area<=0).
func hybridCoverScore(score sql.NullFloat64, bboxJSON string, w, h sql.NullInt64) float64 {
	if !score.Valid || !w.Valid || !h.Valid || w.Int64 <= 0 || h.Int64 <= 0 {
		return -1
	}
	var bb struct {
		X1 float64 `json:"x1"`
		Y1 float64 `json:"y1"`
		X2 float64 `json:"x2"`
		Y2 float64 `json:"y2"`
	}
	if json.Unmarshal([]byte(bboxJSON), &bb) != nil {
		return -1
	}
	area := (bb.X2 - bb.X1) * (bb.Y2 - bb.Y1)
	if area <= 0 {
		return -1
	}
	ratio := area / float64(w.Int64*h.Int64)
	if ratio > 1 {
		ratio = 1 // video bbox is based on the keyframe, EXIF is the original video size, so the ratio can overflow — clamping is enough
	}
	aest := (score.Float64 - 1) / 9
	if aest < 0 {
		aest = 0
	} else if aest > 1 {
		aest = 1
	}
	return aest * ratio
}

// FaceThumbnail crops a square face out of the original image using
// cover_face's bbox, caches it in cacheDir, and returns the file path.
// Returns the cached path directly if already cached. Returns ErrNotFound if
// the person has no cover_face or doesn't exist. Video sources use the
// already-generated large.jpg thumbnail (face detection also ran on the
// keyframe, so the bbox, once normalized, is consistent with the thumbnail's
// ratio); image sources still use the original file to preserve sharpness.
func (s *PersonService) FaceThumbnail(personID, cacheDir, thumbDir string) (string, error) {
	var faceID, bbox, srcPath, mimeType, assetID string
	var origW, origH sql.NullInt64
	err := s.db.QueryRow(`
SELECT fd.id, fd.bbox, a.file_path, COALESCE(a.mime_type,''), a.id, e.width, e.height
FROM persons p
JOIN face_detections fd ON fd.id=p.cover_face_id
JOIN assets a ON a.id=fd.asset_id AND a.offline=0 AND a.deleted_at IS NULL
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
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound // source file is gone (drive unplugged/deleted): externally this is a 404, not a 500
		}
		return "", fmt.Errorf("FaceThumbnail open: %w", err)
	}
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	// bbox is in absolute pixel coordinates on the ML input image (video
	// keyframe or original image). Once a video is compressed into the
	// large.jpg thumbnail (max long side 1280px), the bbox must be scaled by
	// the thumb/original ratio. For an image source the source is the
	// original, so sx=sy=1 (unless EXIF W/H is missing, in which case it
	// falls back to 1:1 and the bbox is treated as pixels of the current
	// image).
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
