// Tests for ProfileStore: full-replace semantics of the user_profile_entities table.
// Covers: ReplaceEntities idempotency/cross-kind isolation, ListEntities ordering.
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── ProfileEntityID: stable derived id ──────────────────────────────────

func TestProfileEntityID(t *testing.T) {
	id1 := ProfileEntityID("pet", "beagle")
	id2 := ProfileEntityID("pet", "beagle")
	require.Equal(t, id1, id2, "same kind+key should get the same id")
	require.Len(t, id1, 16, "should be the first 16 hex chars of sha1")

	require.NotEqual(t, id1, ProfileEntityID("pet", "labrador"), "different key should change the id")
	require.NotEqual(t, id1, ProfileEntityID("person", "beagle"), "different kind should change the id")
}

// ── ReplaceEntities: idempotent full replace + cross-kind isolation ─────

func TestProfileStore_ReplaceEntitiesIdempotentAndIsolatesKind(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	now := time.Now()
	pets := []ProfileEntity{
		{ID: ProfileEntityID("pet", "beagle"), Kind: "pet", Key: "beagle", Label: "Beagle", EvidenceJSON: `{"photo_count":14}`, PhotoCount: 14, FirstSeen: now.AddDate(0, -6, 0), LastSeen: now},
	}
	require.NoError(t, store.ReplaceEntities("pet", pets))

	// Cross-kind isolation uses the kinds from the spec's vocabulary ('pet' |
	// 'person') — family group photos never land in this table (per the
	// design spec's first section: family mining only writes an entity row
	// per named person; "group photo" is the output of a different moments
	// table, not routed through ProfileStore). person is only borrowed here
	// as an isolation control; it doesn't imply the family engine calls this
	// path.
	persons := []ProfileEntity{
		{ID: ProfileEntityID("person", "p1"), Kind: "person", Key: "p1", Label: "Alice", PhotoCount: 40},
	}
	require.NoError(t, store.ReplaceEntities("person", persons))

	// Cross-kind isolation: writing person should not affect pet.
	petList, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList, 1)
	require.Equal(t, "beagle", petList[0].Key)

	personList, err := store.ListEntities("person")
	require.NoError(t, err)
	require.Len(t, personList, 1)
	require.Equal(t, "p1", personList[0].Key)

	// Idempotent full replace: writing pet again (shrunk to an empty set)
	// should clear that kind without affecting person.
	require.NoError(t, store.ReplaceEntities("pet", nil))
	petList2, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList2, 0, "replacing with an empty set should clear all entities of that kind")

	personList2, err := store.ListEntities("person")
	require.NoError(t, err)
	require.Len(t, personList2, 1, "replacing pet should not affect person")

	// Writing a different pet set again should fully replace the old set (not merge with it).
	require.NoError(t, store.ReplaceEntities("pet", []ProfileEntity{
		{ID: ProfileEntityID("pet", "corgi"), Kind: "pet", Key: "corgi", Label: "Corgi", PhotoCount: 20},
	}))
	petList3, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList3, 1)
	require.Equal(t, "corgi", petList3[0].Key)
}

// ── ListEntities: ordering (photo_count descending) ─────────────────────

func TestProfileStore_ListEntitiesOrder(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	entities := []ProfileEntity{
		{ID: ProfileEntityID("pet", "beagle"), Kind: "pet", Key: "beagle", PhotoCount: 14},
		{ID: ProfileEntityID("pet", "corgi"), Kind: "pet", Key: "corgi", PhotoCount: 40},
		{ID: ProfileEntityID("pet", "husky"), Kind: "pet", Key: "husky", PhotoCount: 8},
	}
	require.NoError(t, store.ReplaceEntities("pet", entities))

	list, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Equal(t, "corgi", list[0].Key, "highest photo_count should be first")
	require.Equal(t, "beagle", list[1].Key)
	require.Equal(t, "husky", list[2].Key)
}

// ── evidence/first_seen/last_seen round-trip ─────────────────────────────

func TestProfileStore_ReplaceEntitiesRoundTripsFields(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	first := time.Date(2011, 8, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.ReplaceEntities("pet", []ProfileEntity{
		{
			ID:           ProfileEntityID("pet", "beagle"),
			Kind:         "pet",
			Key:          "beagle",
			Label:        "Beagle",
			EvidenceJSON: `{"photo_count":14,"months":["2011-08","2026-07"]}`,
			PhotoCount:   14,
			FirstSeen:    first,
			LastSeen:     last,
		},
	}))

	list, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, list, 1)
	e := list[0]
	require.Equal(t, "Beagle", e.Label)
	require.Equal(t, `{"photo_count":14,"months":["2011-08","2026-07"]}`, e.EvidenceJSON)
	require.Equal(t, 14, e.PhotoCount)
	require.True(t, e.FirstSeen.Equal(first), "first_seen should round-trip consistently")
	require.True(t, e.LastSeen.Equal(last), "last_seen should round-trip consistently")
	require.Greater(t, e.UpdatedAt, int64(0), "updated_at should be written by Store as Unix ms")
}
