// Tests for family profile mining + the entity moment engine: covers the
// Step 1 brief checklist — two people each qualify by appearance frequency,
// enough co-occurring photos among them qualify a "together" collection,
// photos with only 1 of them present don't count toward it; a named person
// ("Alice", qualifying) produces an "Alice Through the Years" draft, an
// unnamed person gets no individual draft; person entities persist to the
// profile table (including unnamed ones, with an empty label); hidden
// persons are excluded; a person with frequency < MinPersonPhotos doesn't
// become an entity.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// familyTestRecipe assembles a kind=family test recipe.
func familyTestRecipe(topPersons, minPersonPhotos, minTogetherPersons, minAssets int) MomentRecipe {
	params := RecipeParams{
		TopPersons:         topPersons,
		MinPersonPhotos:    minPersonPhotos,
		MinTogetherPersons: minTogetherPersons,
		MinAssets:          minAssets,
	}
	b, _ := json.Marshal(params)
	return MomentRecipe{Key: "profile:family", Kind: "family", Title: "Family Entities", ParamsJSON: string(b)}
}

// insertPerson inserts a persons row; name can be empty (unnamed), hidden is
// controllable.
func insertPerson(t *testing.T, db *sql.DB, id, name string, hidden bool) {
	t.Helper()
	h := 0
	if hidden {
		h = 1
	}
	_, err := db.Exec(`INSERT INTO persons(id, name, hidden) VALUES(?,?,?)`, id, name, h)
	require.NoError(t, err)
}

// attachFace inserts a face for an asset and binds it to personID; excluded
// is controllable.
func attachFace(t *testing.T, db *sql.DB, faceID, assetID, personID string, excluded bool) {
	t.Helper()
	ex := 0
	if excluded {
		ex = 1
	}
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding, excluded) VALUES(?,?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, []byte{0}, ex)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?,?)`, faceID, personID)
	require.NoError(t, err)
}

// ── MinePersonEntities: qualification threshold + hidden exclusion ─────────

func TestMinePersonEntities_ThresholdAndHiddenExclusion(t *testing.T) {
	db := makeTestDB(t)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)

	// Alice: 35 photos, qualifies (min_person_photos=30).
	insertPerson(t, db, "alice", "Alice", false)
	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}

	// Bob: only 5 photos, doesn't qualify.
	insertPerson(t, db, "bob", "Bob", false)
	for i := 0; i < 5; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}

	// Carol: 35 photos but hidden=1, should be excluded (not made an entity,
	// even though the frequency qualifies).
	insertPerson(t, db, "carol", "Carol", true)
	for i := 0; i < 35; i++ {
		id := "carol-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "carol-face-"+itoa(i), id, "carol", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	entities, err := MinePersonEntities(context.Background(), db, recipe)
	require.NoError(t, err)

	byKey := map[string]ProfileEntity{}
	for _, e := range entities {
		byKey[e.Key] = e
	}

	require.Contains(t, byKey, "alice")
	require.Equal(t, "person", byKey["alice"].Kind)
	require.Equal(t, 35, byKey["alice"].PhotoCount)
	require.Equal(t, "Alice", byKey["alice"].Label)

	require.NotContains(t, byKey, "bob", "5 photos isn't enough to qualify (min_person_photos=30)")
	require.NotContains(t, byKey, "carol", "a hidden person should be excluded")
}

// ── MinePersonEntities: an excluded face doesn't count toward frequency ────

func TestMinePersonEntities_ExcludedFaceNotCounted(t *testing.T) {
	db := makeTestDB(t)
	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "dave", "Dave", false)
	for i := 0; i < 29; i++ {
		id := "dave-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "dave-face-"+itoa(i), id, "dave", false)
	}
	// The 30th face is excluded and should not count toward frequency (still 1
	// photo short of qualifying).
	insertThemeAsset(t, db, "dave-asset-excl", base.AddDate(0, 0, 30))
	attachFace(t, db, "dave-face-excl", "dave-asset-excl", "dave", true)

	recipe := familyTestRecipe(5, 30, 2, 10)
	entities, err := MinePersonEntities(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Empty(t, entities, "an excluded face should not count toward frequency, 29 photos isn't enough to qualify")
}

// ── BuildFamilyMoments: the "together" draft ────────────────────────────────

func TestBuildFamilyMoments_TogetherDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.January, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "bob", "Bob", false)

	// Alice and Bob each qualify by frequency (35 photos each, including 12
	// where they co-occur).
	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 100+i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}
	// 12 photos where they co-occur (both Alice + Bob present): the "together"
	// collection should qualify (min_assets=10).
	for i := 0; i < 12; i++ {
		id := "together-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 200+i))
		attachFace(t, db, "together-a-face-"+itoa(i), id, "alice", false)
		attachFace(t, db, "together-b-face-"+itoa(i), id, "bob", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	var together *MomentDraft
	for i := range drafts {
		if drafts[i].ID == ProfileEntityID("family", "together") {
			together = &drafts[i]
		}
	}
	require.NotNil(t, together, "qualifying co-occurrence should produce a 'together' collection")
	require.Equal(t, "Family Moments", together.Title)
	require.NotEmpty(t, together.Subtitle)
	require.Len(t, together.Assets, 12)
}

func TestBuildFamilyMoments_InsufficientTogetherNoDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.January, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "bob", "Bob", false)

	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 100+i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}
	// Only 1 co-occurring photo (< min_assets=10), the "together" collection
	// should not be produced.
	insertThemeAsset(t, db, "together-asset-0", base.AddDate(0, 0, 200))
	attachFace(t, db, "together-a-face-0", "together-asset-0", "alice", false)
	attachFace(t, db, "together-b-face-0", "together-asset-0", "bob", false)

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	for _, d := range drafts {
		require.NotEqual(t, ProfileEntityID("family", "together"), d.ID, "only 1 co-occurring photo isn't enough to produce a 'together' collection")
	}
}

// ── BuildFamilyMoments: the named-person draft ──────────────────────────────

func TestBuildFamilyMoments_NamedPersonDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2015, time.June, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "unnamed1", "", false) // unnamed, but also qualifies by frequency

	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "unnamed-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "unnamed-face-"+itoa(i), id, "unnamed1", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	var alice *MomentDraft
	for i := range drafts {
		if drafts[i].ID == ProfileEntityID("person", "alice") {
			alice = &drafts[i]
		}
	}
	require.NotNil(t, alice, "a named, qualifying person should produce a Through the Years draft")
	require.Equal(t, "Alice Through the Years", alice.Title)
	require.Equal(t, 35, alice.AssetCount)

	for _, d := range drafts {
		require.NotEqual(t, ProfileEntityID("person", "unnamed1"), d.ID, "an unnamed person should not produce an individual draft")
	}

	// Person entities persist to the profile table, including unnamed ones
	// (empty label).
	saved, err := profileStore.ListEntities("person")
	require.NoError(t, err)
	byKey := map[string]ProfileEntity{}
	for _, e := range saved {
		byKey[e.Key] = e
	}
	require.Contains(t, byKey, "alice")
	require.Equal(t, "Alice", byKey["alice"].Label)
	require.Contains(t, byKey, "unnamed1", "an unnamed person should also persist to the profile table (entity mining doesn't exclude for being unnamed)")
	require.Equal(t, "", byKey["unnamed1"].Label)
}

// ── BuildFamilyMoments: no qualifying entity clears the profile ─────────────

func TestBuildFamilyMoments_NoQualifyingClearsProfile(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	// Stale profile: simulates a previous round that qualified.
	require.NoError(t, profileStore.ReplaceEntities("person", []ProfileEntity{
		{ID: ProfileEntityID("person", "stale"), Kind: "person", Key: "stale", Label: "Stale", PhotoCount: 40},
	}))

	insertPerson(t, db, "bob", "Bob", false)
	insertThemeAsset(t, db, "bob-asset-0", time.Now())
	attachFace(t, db, "bob-face-0", "bob-asset-0", "bob", false)

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)

	saved, err := profileStore.ListEntities("person")
	require.NoError(t, err)
	require.Empty(t, saved, "no qualifying entity should clear the previous round's profile, leaving no stale data")
}

// itoa is a minimal int-to-string helper (to avoid dependency noise beyond
// strconv in tests; using strconv directly would be just as clear, this
// wrapper is kept only to keep call sites a bit shorter).
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
