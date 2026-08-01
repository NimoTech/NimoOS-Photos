// Moments M2 profile-layer data layer: repo for the single
// user_profile_entities table.
//
// A profile entity is a mined result about "the user's own pets/family"
// (as opposed to M1's concept-version theme moments, which do a "library-wide
// search for pet-containing elements") — mining happens inside the engine
// build for the corresponding recipe (profile:pets/profile:family), on the
// same transaction cadence as recompute, and produces a full replace by kind
// (delete all rows of that kind + insert), idempotent, isolated across kinds
// (replacing pet never affects family, and vice versa).
package service

import (
	"database/sql"
	"fmt"
	"time"
)

// ProfileEntity corresponds to one row of user_profile_entities. EvidenceJSON
// is a JSON snapshot of the mining evidence (photo_count/months/first/last,
// etc.), kept for troubleshooting and future upgrades to read — it is never
// used for query filtering. FirstSeen/LastSeen being the zero time.Time means
// that column is NULL in the DB.
type ProfileEntity struct {
	ID           string
	Kind         string // "pet" | "person" (place/activity reserved)
	Key          string // pet: species term e.g. 'beagle'; person: person_id
	Label        string
	EvidenceJSON string
	PhotoCount   int
	FirstSeen    time.Time
	LastSeen     time.Time
	UpdatedAt    int64 // Unix ms
}

// ProfileEntityID derives a stable id for a profile entity: the first 16 hex
// chars of hash(kind + "|" + key). Same approach as TripMomentID/ThemeMomentID
// (see momentstore.go's hashID16) — a given kind+key always maps to the same
// row, so a recompute just refreshes it in place.
func ProfileEntityID(kind, key string) string {
	return hashID16(kind + "|" + key)
}

// ProfileStore is the repo layer for the user_profile_entities table, plain
// SQL, no ORM (following MomentStore's style).
type ProfileStore struct {
	db *sql.DB
}

// NewProfileStore constructs a ProfileStore.
func NewProfileStore(db *sql.DB) *ProfileStore {
	return &ProfileStore{db: db}
}

// ReplaceEntities is the idempotent full-replace entry point for profile
// entities of a given kind: within a transaction it first deletes all
// existing rows of that kind, then inserts this round's entities. Mining
// results are not merged incrementally — each mining pass re-evaluates the
// qualifying set library-wide for that kind, and a full replace avoids
// leaving behind stale entities that "used to qualify but no longer do".
// Passing an empty entities slice is equivalent to clearing the kind (e.g.
// when this pass found nothing qualifying library-wide). Rows of other kinds
// are unaffected (WHERE kind=? strictly isolates them).
func (s *ProfileStore) ReplaceEntities(kind string, entities []ProfileEntity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("profile: replace entities begin: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM user_profile_entities WHERE kind=?`, kind); err != nil {
		tx.Rollback()
		return fmt.Errorf("profile: clear kind %q: %w", kind, err)
	}

	now := nowMs()
	for _, e := range entities {
		if _, err := tx.Exec(`
			INSERT INTO user_profile_entities(id, kind, key, label, evidence, photo_count, first_seen, last_seen, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, kind, e.Key, e.Label, nonEmptyOr(e.EvidenceJSON, "{}"), e.PhotoCount,
			nullTimeArg(e.FirstSeen), nullTimeArg(e.LastSeen), now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("profile: insert entity %q/%q: %w", kind, e.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("profile: replace entities commit: %w", err)
	}
	return nil
}

// nonEmptyOr falls back to fallback when s is an empty string (the evidence
// column is NOT NULL DEFAULT '{}', so a caller that omits EvidenceJSON should
// land on '{}' rather than an empty string).
func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ListEntities lists every profile entity of a given kind, ordered by
// photo_count descending (the entities with the strongest presence come
// first, e.g. the default order when the UI shows "your pets").
func (s *ProfileStore) ListEntities(kind string) ([]ProfileEntity, error) {
	rows, err := s.db.Query(`
		SELECT id, kind, key, label, evidence, photo_count, first_seen, last_seen, updated_at
		FROM user_profile_entities WHERE kind=? ORDER BY photo_count DESC`, kind)
	if err != nil {
		return nil, fmt.Errorf("profile: list entities %q: %w", kind, err)
	}
	defer rows.Close()

	var out []ProfileEntity
	for rows.Next() {
		var e ProfileEntity
		var first, last sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &e.Label, &e.EvidenceJSON, &e.PhotoCount, &first, &last, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("profile: scan entity: %w", err)
		}
		if t := parseSQLiteTime(first); t != nil {
			e.FirstSeen = *t
		}
		if t := parseSQLiteTime(last); t != nil {
			e.LastSeen = *t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
