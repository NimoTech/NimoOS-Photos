package service_test

import (
	"database/sql"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// insertHiddenTestPerson inserts a bare persons row for ListHiddenPersons
// tests. Face clustering is irrelevant here — only the persons table's
// hidden/purge_at/updated_at state matters — so this bypasses it entirely.
func insertHiddenTestPerson(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO persons(id, name, confidence, cover_face_id) VALUES(?, ?, 0.9, ?)`,
		id, name, "face-"+id)
	require.NoError(t, err)
}

// A plainly-hidden person (HidePerson, no purge_at) must appear in the
// hidden list with its light fields populated.
func TestListHiddenPersons_PlainHiddenAppears(t *testing.T) {
	db := makeTestFaceDB(t)
	insertHiddenTestPerson(t, db, "h-plain", "Plain Hidden")
	ps := service.NewPersonService(db)

	require.NoError(t, ps.HidePerson("h-plain"))

	list, err := ps.ListHiddenPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "h-plain", list[0].ID)
	require.Equal(t, "Plain Hidden", list[0].Name)
	require.Equal(t, "face-h-plain", list[0].CoverFaceID)
	require.Equal(t, 0.9, list[0].Confidence)
}

// A person hidden via HidePersonForPurge (grace-period pending purge) must
// NOT appear in the hidden list — it's "being deleted", not "hidden", and
// belongs to the sweep's domain instead.
func TestListHiddenPersons_GracePeriodPendingPurgeExcluded(t *testing.T) {
	db := makeTestFaceDB(t)
	insertHiddenTestPerson(t, db, "h-grace", "Grace Period")
	ps := service.NewPersonService(db)

	require.NoError(t, ps.HidePersonForPurge("h-grace"))

	list, err := ps.ListHiddenPersons()
	require.NoError(t, err)
	require.Empty(t, list, "a person pending purge must not show up as merely hidden")
}

// Restoring a plainly-hidden person must remove it from the hidden list.
func TestListHiddenPersons_RestoredDisappears(t *testing.T) {
	db := makeTestFaceDB(t)
	insertHiddenTestPerson(t, db, "h-restore", "Restored")
	ps := service.NewPersonService(db)

	require.NoError(t, ps.HidePerson("h-restore"))
	list, err := ps.ListHiddenPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)

	require.NoError(t, ps.RestorePerson("h-restore"))
	list2, err := ps.ListHiddenPersons()
	require.NoError(t, err)
	require.Empty(t, list2)
}
