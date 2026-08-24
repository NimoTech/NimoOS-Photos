package service_test

// Tests for Task 7 (final task) of the exemplar-assignment plan: the
// one-shot migration's marker-guarded "lossless first pass" behavior in
// rebuildPersonsWithProgress's step 1.5 revalidation (service/faces.go), and
// the RunExemplarMigration startup driver (service/exemplar_migrate.go).
//
// Reuses makeTestFaceDB/insertAssetFace (faces_test.go),
// setClusterEngine (faces_engine_test.go), vecAtDist/
// setupAnchoredExemplarPerson (faces_assign_test.go), and
// linkMember/membership/reviewSuggestionRow (faces_revalidate_test.go) --
// all same package.
//
import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// exemplarMarkerFileForTest is a deliberate, hardcoded duplicate of service's
// private exemplarInitMarkerFile constant (".exemplar_init_v1.done") -- this
// filename is a stable, documented contract (see OVERVIEW.md's
// exemplar-assignment migration section and task-7-brief.md), the same way
// these tests already reach past the package boundary to assert on raw
// person_suggestions rows via SQL.
const exemplarMarkerFileForTest = ".exemplar_init_v1.done"

// markerExists reports whether the exemplar-assignment migration marker file
// exists in dir.
func markerExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, exemplarMarkerFileForTest))
	return err == nil
}

// TestExemplarMigration_LosslessFirstPassThenNormalSecondPass is the core
// Task 7 case: with a marker directory configured but no marker file yet, a
// member beyond assignSuggestDist is demoted to an open 'review' suggestion
// (kept, not detached) on the first RunClustering call, and the marker gets
// written; a second RunClustering call (marker now present) detaches the
// still-drifted member normally, exactly like the pre-Task-7 behavior.
func TestExemplarMigration_LosslessFirstPassThenNormalSecondPass(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9) // well beyond the default 0.60 suggestDist
	driftFaceID := insertAssetFace(t, db, "migrate-drift-1", driftVec)
	linkMember(t, db, driftFaceID, pid, 0)

	markerDir := t.TempDir()
	require.False(t, markerExists(t, markerDir), "marker must not exist before the migration's first pass")

	svc := service.NewFaceService(db)
	svc.SetMarkerDir(markerDir)

	// Pass 1: lossless first pass -- kept, demoted to review, marker written.
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, driftFaceID)
	require.True(t, found, "the migration's first pass must never silently drop a member")
	require.Equal(t, pid, gotPid, "a drifted member must be KEPT (not detached) during the lossless first pass")
	require.False(t, confirmed)

	exists, status, score := reviewSuggestionRow(t, db, pid, driftFaceID)
	require.True(t, exists, "expected an open 'review' suggestion demoting the drifted member instead of detaching it")
	require.Equal(t, "open", status)
	require.Greater(t, score, 0.0)

	require.True(t, markerExists(t, markerDir), "the marker must be written once the lossless first pass commits")

	// Pass 2: marker now present -- normal (non-lossless) revalidation
	// applies, so the still-drifted member is detached like any other pass.
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid2, _, found2 := membership(t, db, driftFaceID)
	require.True(t, found2, "a detached face must still land somewhere via two-pass clustering this same pass")
	require.NotEqual(t, pid, gotPid2, "the second pass must detach the still-drifted member normally")
}

// TestExemplarMigration_MarkerAlreadyPresentIsNormalFromStart proves that
// when the marker file already exists BEFORE any clustering pass runs (e.g.
// a fresh deploy of a build that ships with the marker pre-seeded, or a
// second service instance started after another already completed the
// migration), revalidation behaves normally from the very first call --
// never lossless.
func TestExemplarMigration_MarkerAlreadyPresentIsNormalFromStart(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9)
	driftFaceID := insertAssetFace(t, db, "migrate-drift-2", driftVec)
	linkMember(t, db, driftFaceID, pid, 0)

	markerDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(markerDir, exemplarMarkerFileForTest), []byte("pre-seeded\n"), 0o644))

	svc := service.NewFaceService(db)
	svc.SetMarkerDir(markerDir)

	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, driftFaceID)
	require.True(t, found, "a detached face must still land somewhere via two-pass clustering this same pass")
	require.NotEqual(t, pid, gotPid, "with the marker already present, the very first pass must detach normally, not demote")
}

// TestRunExemplarMigration_NoOpWithoutMarkerDir proves RunExemplarMigration
// is a harmless no-op for a FaceService that never called SetMarkerDir (the
// default for most tests, and for any deployment that somehow skipped
// service.NewService's wiring).
func TestRunExemplarMigration_NoOpWithoutMarkerDir(t *testing.T) {
	db := makeTestFaceDB(t)
	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunExemplarMigration(context.Background()))
}

// TestRunExemplarMigration_RunsClusteringAndWritesMarkerOnce proves the
// startup driver actually triggers a clustering pass (populating `persons`)
// and leaves the marker behind, and that a second call is a cheap no-op
// (doesn't error, marker stays in place).
func TestRunExemplarMigration_RunsClusteringAndWritesMarkerOnce(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512
	vec, _ := vecAtDist(dim, 0.05)
	insertAssetFaceQuality(t, db, "migrate-seed-1", vec, 0.9, 0.9, 0.9)

	markerDir := t.TempDir()
	svc := service.NewFaceService(db)
	svc.SetMarkerDir(markerDir)

	require.NoError(t, svc.RunExemplarMigration(context.Background()))

	var personCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
	require.Greater(t, personCount, 0, "RunExemplarMigration must have triggered a real clustering pass")
	require.True(t, markerExists(t, markerDir))

	// Second call: marker present, must stay a no-op (no error).
	require.NoError(t, svc.RunExemplarMigration(context.Background()))
}
