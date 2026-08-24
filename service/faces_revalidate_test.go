package service_test

// Tests for Task 5 of the exemplar-assignment plan: per-pass revalidation of
// already-anchored, unconfirmed members against their OWN person's exemplar
// set (service/faces.go's rebuildPersonsWithProgress, inserted between step
// 1's exemplar build and step 2's auto-person deletion). This is the
// drift-killer for "members can enter but never leave" -- prior to this,
// nothing ever re-checked an existing member once it had joined.
//
// Reuses makeTestFaceDB/insertAssetFace/normalize (faces_test.go),
// setClusterEngine/personOfFace (faces_engine_test.go), and
// vecAtDist/insertAssetFaceQuality/setupAnchoredExemplarPerson/membership
// (faces_assign_test.go) -- all same package.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// reviewSuggestionRow reports whether an open 'review' suggestion exists for
// (personID, faceID), and its stored score -- the revalidation-gray-zone
// counterpart to faces_assign_test.go's suggestionRow (which is hardcoded to
// kind='join').
func reviewSuggestionRow(t *testing.T, db *sql.DB, personID, faceID string) (exists bool, status string, score float64) {
	t.Helper()
	row := db.QueryRow(`SELECT status, score FROM person_suggestions WHERE person_id=? AND face_id=? AND kind='review'`,
		personID, faceID)
	err := row.Scan(&status, &score)
	if err == sql.ErrNoRows {
		return false, "", 0
	}
	require.NoError(t, err)
	return true, status, score
}

// negativeExists reports whether a person_negatives row exists for the pair.
func negativeExists(t *testing.T, db *sql.DB, personID, faceID string) bool {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM person_negatives WHERE person_id=? AND face_id=?`, personID, faceID).Scan(&count))
	return count > 0
}

// reviewSuggestionRowFull is reviewSuggestionRow plus kind/created_at/
// decided_at, needed by the Fix round 1 tests to confirm a decided row's
// FULL contents -- not just status -- survive an untouched later pass.
func reviewSuggestionRowFull(t *testing.T, db *sql.DB, personID, faceID string) (
	exists bool, status string, score float64, createdAt, decidedAt sql.NullTime,
) {
	t.Helper()
	row := db.QueryRow(`SELECT status, score, created_at, decided_at FROM person_suggestions
		WHERE person_id=? AND face_id=? AND kind='review'`, personID, faceID)
	err := row.Scan(&status, &score, &createdAt, &decidedAt)
	if err == sql.ErrNoRows {
		return false, "", 0, sql.NullTime{}, sql.NullTime{}
	}
	require.NoError(t, err)
	return true, status, score, createdAt, decidedAt
}

// countReviewSuggestions counts person_suggestions rows for (personID,
// faceID, kind='review') -- used to confirm the UPSERT never creates a
// duplicate row (the UNIQUE(person_id, face_id) index should make this
// impossible regardless, but the Fix round 1 tests pin it explicitly since a
// WHERE-guarded DO UPDATE is new surface here).
func countReviewSuggestions(t *testing.T, db *sql.DB, personID, faceID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM person_suggestions WHERE person_id=? AND face_id=? AND kind='review'`,
		personID, faceID).Scan(&n))
	return n
}

// seedReviewSuggestion pre-inserts a person_suggestions row with an explicit
// id/kind/score/status/created_at/decided_at -- used by the Fix round 1
// tests to simulate a suggestion some earlier pass already produced (and,
// for the decided cases, that a user has since accepted/rejected) before a
// later RunClustering pass runs the revalidation UPSERT against the same
// (person_id, face_id) pair.
func seedReviewSuggestion(t *testing.T, db *sql.DB, personID, faceID, status string, score float64, decidedAt *time.Time) string {
	t.Helper()
	id := uuid.NewString()
	createdAt := time.Now().Add(-time.Hour) // deliberately stale, distinct from "now"
	var decided any
	if decidedAt != nil {
		decided = *decidedAt
	}
	_, err := db.Exec(`
		INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at, decided_at)
		VALUES(?, ?, ?, 'review', ?, ?, ?, ?)`,
		id, personID, faceID, score, status, createdAt, decided)
	require.NoError(t, err)
	return id
}

// linkMember directly inserts a face_person row for (faceID, personID),
// bypassing the normal Match-driven assignment path -- these tests need to
// simulate a member that ALREADY belongs to a person (e.g. from some earlier
// pass, before the person's exemplar set drifted or was recomputed) rather
// than one that just joined via KNN voting.
func linkMember(t *testing.T, db *sql.DB, faceID, personID string, confirmed int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO face_person(face_id, person_id, confirmed) VALUES(?,?,?)`,
		faceID, personID, confirmed)
	require.NoError(t, err)
}

// TestRunClustering_AppleRevalidate_DriftedMemberDetachedAndRejoinsFreePool
// is brief case ①: an unconfirmed member whose distance to its own person's
// exemplars is beyond assignSuggestDist is detached from that person AND
// flows into this same pass's free-face handling (here: forms its own new
// auto person via step 4's two-pass clustering, since it has no other close
// neighbor). No person_negatives row is written -- auto-eviction is not a
// user denial.
func TestRunClustering_AppleRevalidate_DriftedMemberDetachedAndRejoinsFreePool(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9) // well beyond the default 0.60 suggestDist
	driftFaceID := insertAssetFace(t, db, "drift-1", driftVec)
	linkMember(t, db, driftFaceID, pid, 0)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, driftFaceID)
	require.True(t, found, "a detached face must still land somewhere via two-pass clustering this same pass")
	require.NotEqual(t, pid, gotPid, "a drifted member must be detached from its stale person")

	require.False(t, negativeExists(t, db, pid, driftFaceID),
		"algorithmic auto-eviction must not write a person_negatives row (auto-removal != user denial)")
}

// TestRunClustering_AppleRevalidate_GrayZoneMemberKeptWithReviewSuggestion is
// brief case ②: an unconfirmed member landing strictly between assignAutoDist
// and assignSuggestDist against its own person's exemplars keeps its
// membership, but an open 'review' suggestion is queued for it.
func TestRunClustering_AppleRevalidate_GrayZoneMemberKeptWithReviewSuggestion(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, grayVec := vecAtDist(dim, 0.5) // between the default 0.45/0.60 bounds
	grayFaceID := insertAssetFace(t, db, "gray-1", grayVec)
	linkMember(t, db, grayFaceID, pid, 0)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, grayFaceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "a gray-zone member must keep its membership, not be detached")
	require.False(t, confirmed)

	exists, status, score := reviewSuggestionRow(t, db, pid, grayFaceID)
	require.True(t, exists, "expected an open 'review' suggestion for the gray-zone member")
	require.Equal(t, "open", status)
	require.Greater(t, score, 0.0)
}

// TestRunClustering_AppleRevalidate_RecoveredMemberResolvesOpenReviewSuggestion
// is the CRITICAL fix from the final whole-span review: a member that had
// drifted into the gray zone on some earlier pass (and got an open 'review'
// row) but is, THIS pass, squarely back within the auto band must have that
// now-moot open row resolved (deleted). Left open, the system itself just
// re-confirmed this member is fine, yet the stale card would let a human
// reject it later -- detaching an otherwise still-good member AND
// permanently negating it, since no un-negate surface exists.
func TestRunClustering_AppleRevalidate_RecoveredMemberResolvesOpenReviewSuggestion(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	// Squarely within the auto band (identical to the person's own
	// exemplars), but carrying a stale open 'review' row as if an earlier
	// pass had it in the gray zone.
	recoveredFaceID := insertAssetFace(t, db, "recovered-1", baseVec)
	linkMember(t, db, recoveredFaceID, pid, 0)
	seedReviewSuggestion(t, db, pid, recoveredFaceID, "open", 0.55, nil)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, recoveredFaceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "a recovered member must stay a member")
	require.False(t, confirmed)

	exists, _, _ := reviewSuggestionRow(t, db, pid, recoveredFaceID)
	require.False(t, exists, "the now-moot open review row must be resolved (deleted), not left dangling")
}

// TestRunClustering_AppleRevalidate_DecidedSuggestionNeverDeletedOnAutoRecovery
// is the guard's mirror: a DECIDED (accepted/rejected) suggestion row for a
// member that resolves to "auto" this pass must never be deleted by the new
// resolve-on-auto path -- only OPEN rows are ephemeral machine questions; a
// decided row is a real user decision and stays as an audit trail.
func TestRunClustering_AppleRevalidate_DecidedSuggestionNeverDeletedOnAutoRecovery(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "decided-auto-1", baseVec)
	linkMember(t, db, faceID, pid, 0)
	decidedAt := time.Now().Add(-time.Hour)
	seedReviewSuggestion(t, db, pid, faceID, "rejected", 0.55, &decidedAt)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	exists, status, _ := reviewSuggestionRow(t, db, pid, faceID)
	require.True(t, exists, "a decided row must never be deleted by the auto-resolve path")
	require.Equal(t, "rejected", status)
}

// TestRunClustering_AppleRevalidate_RejectedSuggestionNeverSilentlyReopened
// is Fix round 1 case (a): a gray-zone member with a pre-existing REJECTED
// review suggestion must not have that row silently flipped back to 'open'
// by a later pass's UPSERT -- status, score, and decided_at must all stay
// exactly as the (simulated) user decision left them, and no duplicate row
// must appear. This is the core regression this fix round closes: without
// the DO UPDATE's `WHERE status='open'` guard, the UPSERT would have reset
// status to 'open' while leaving the stale decided_at in place -- an
// internally inconsistent row (decided timestamp on a row that claims to be
// still pending).
func TestRunClustering_AppleRevalidate_RejectedSuggestionNeverSilentlyReopened(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, grayVec := vecAtDist(dim, 0.5) // gray zone -- would normally produce/refresh a 'review' row
	grayFaceID := insertAssetFace(t, db, "gray-rejected", grayVec)
	linkMember(t, db, grayFaceID, pid, 0)

	decidedAt := time.Now().Add(-30 * time.Minute)
	seedReviewSuggestion(t, db, pid, grayFaceID, "rejected", 0.999, &decidedAt)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	exists, status, score, _, gotDecidedAt := reviewSuggestionRowFull(t, db, pid, grayFaceID)
	require.True(t, exists)
	require.Equal(t, "rejected", status, "a rejected suggestion must never be silently reopened")
	require.Equal(t, 0.999, score, "a decided row's score must not be overwritten by a later pass")
	require.True(t, gotDecidedAt.Valid)
	require.WithinDuration(t, decidedAt, gotDecidedAt.Time, time.Second,
		"a decided row's decided_at must not be touched by a later pass")
	require.Equal(t, 1, countReviewSuggestions(t, db, pid, grayFaceID), "no duplicate row must be created")
}

// TestRunClustering_AppleRevalidate_AcceptedSuggestionNeverSilentlyReopened
// is Fix round 1 case (b): same as the rejected case above, but for an
// ACCEPTED suggestion -- covered separately since T6's accept semantics
// (member becomes confirmed=1, exempt from revalidation entirely) make this
// path unreachable through the normal accept flow, but the write-site guard
// must still hold defensively regardless of how the row got here.
func TestRunClustering_AppleRevalidate_AcceptedSuggestionNeverSilentlyReopened(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, grayVec := vecAtDist(dim, 0.5)
	grayFaceID := insertAssetFace(t, db, "gray-accepted", grayVec)
	linkMember(t, db, grayFaceID, pid, 0)

	decidedAt := time.Now().Add(-30 * time.Minute)
	seedReviewSuggestion(t, db, pid, grayFaceID, "accepted", 0.111, &decidedAt)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	exists, status, score, _, gotDecidedAt := reviewSuggestionRowFull(t, db, pid, grayFaceID)
	require.True(t, exists)
	require.Equal(t, "accepted", status, "an accepted suggestion must never be silently reopened")
	require.Equal(t, 0.111, score, "a decided row's score must not be overwritten by a later pass")
	require.True(t, gotDecidedAt.Valid)
	require.WithinDuration(t, decidedAt, gotDecidedAt.Time, time.Second,
		"a decided row's decided_at must not be touched by a later pass")
	require.Equal(t, 1, countReviewSuggestions(t, db, pid, grayFaceID), "no duplicate row must be created")
}

// TestRunClustering_AppleRevalidate_OpenSuggestionStillRefreshedByLaterPass
// is Fix round 1 case (c): the legitimate update path must keep working --
// an OPEN review suggestion's score (and kind) must still be refreshed by a
// later pass's UPSERT, proving the new WHERE guard only blocks decided rows,
// not open ones.
func TestRunClustering_AppleRevalidate_OpenSuggestionStillRefreshedByLaterPass(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, grayVec := vecAtDist(dim, 0.5)
	grayFaceID := insertAssetFace(t, db, "gray-open", grayVec)
	linkMember(t, db, grayFaceID, pid, 0)

	staleScore := 0.5001 // deliberately distinct from whatever this pass recomputes
	seedReviewSuggestion(t, db, pid, grayFaceID, "open", staleScore, nil)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	exists, status, score, _, gotDecidedAt := reviewSuggestionRowFull(t, db, pid, grayFaceID)
	require.True(t, exists)
	require.Equal(t, "open", status)
	require.NotEqual(t, staleScore, score, "an open row's score must be refreshed by a later pass, not left stale")
	require.Greater(t, score, 0.0)
	require.False(t, gotDecidedAt.Valid, "an open row must never have a decided_at")
	require.Equal(t, 1, countReviewSuggestions(t, db, pid, grayFaceID), "no duplicate row must be created")
}

// TestRunClustering_AppleRevalidate_ConfirmedMemberNeverTouched is brief case
// ③: a confirmed=1 member is never revalidated, no matter how far its
// embedding drifts from its person's exemplars -- neither detached nor
// suggested.
func TestRunClustering_AppleRevalidate_ConfirmedMemberNeverTouched(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9)
	faceID := insertAssetFace(t, db, "confirmed-drift", driftVec)
	linkMember(t, db, faceID, pid, 1)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, faceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "a confirmed member must never be detached, regardless of drift")
	require.True(t, confirmed)

	exists, _, _ := reviewSuggestionRow(t, db, pid, faceID)
	require.False(t, exists, "a confirmed member must never get a review suggestion either")
}

// TestRunClustering_AppleRevalidate_CoverLockedFaceNeverTouched is brief case
// ④ (cover half): the person's pinned cover_locked face is exempt from
// revalidation entirely, even at maximum drift distance.
func TestRunClustering_AppleRevalidate_CoverLockedFaceNeverTouched(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9)
	faceID := insertAssetFace(t, db, "cover-drift", driftVec)
	linkMember(t, db, faceID, pid, 0)

	_, err := db.Exec(`UPDATE persons SET cover_locked=1, cover_face_id=? WHERE id=?`, faceID, pid)
	require.NoError(t, err)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, faceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "the cover_locked face must never be detached")

	exists, _, _ := reviewSuggestionRow(t, db, pid, faceID)
	require.False(t, exists, "the cover_locked face must never get a review suggestion either")
}

// TestRunClustering_AppleRevalidate_HeroAssetFaceNeverTouched is brief case ④
// (hero half): any face on the person's hero_asset_id is exempt from
// revalidation, even at maximum drift distance.
func TestRunClustering_AppleRevalidate_HeroAssetFaceNeverTouched(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	_, driftVec := vecAtDist(dim, 0.9)
	heroAssetID := "hero-drift-asset"
	faceID := insertAssetFace(t, db, heroAssetID, driftVec)
	linkMember(t, db, faceID, pid, 0)

	_, err := db.Exec(`UPDATE persons SET hero_asset_id=? WHERE id=?`, heroAssetID, pid)
	require.NoError(t, err)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, faceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "a face on the person's hero asset must never be detached")

	exists, _, _ := reviewSuggestionRow(t, db, pid, faceID)
	require.False(t, exists, "a face on the person's hero asset must never get a review suggestion either")
}

// TestRunClustering_AppleRevalidate_ExemplarSetRecomputedAfterRemoval is
// brief case ⑤: after revalidation detaches a member from a person, that
// person's exemplar set is recomputed BEFORE step 3's assignment stage reads
// it -- verified two ways: (a) the detached face is gone from face_person
// under this person entirely, and (b) with exemplarCap constrained tightly
// enough that the detached face's presence had displaced a same-quality
// sibling out of the exemplar selection, that sibling becomes exemplar=1
// again once the set is recomputed without the detached face. (b) is the
// part that actually fails if the recompute step were skipped: without it,
// the sibling excluded by the original (pre-removal) cap-3 selection would
// remain exemplar=0 even though it is now one of only 3 remaining members.
func TestRunClustering_AppleRevalidate_ExemplarSetRecomputedAfterRemoval(t *testing.T) {
	setClusterEngine(t, "apple")
	old := config.Cfg
	config.Cfg = &config.Config{ExemplarMaxPerPerson: 3}
	t.Cleanup(func() { config.Cfg = old })

	db := makeTestFaceDB(t)
	dim := 512
	baseVec, driftVec := vecAtDist(dim, 0.9) // baseVec is always e0

	// First pass: only the 3 identical-vector near faces exist, so they
	// cluster into one auto person (identical embeddings -- trivially the
	// same moment cluster) that then gets marked hidden (anchored).
	insertAssetFaceQuality(t, db, "near-a", baseVec, 0.90, 0.9, 0.9)
	insertAssetFaceQuality(t, db, "near-b", baseVec, 0.90, 0.9, 0.9)
	insertAssetFaceQuality(t, db, "near-c", baseVec, 0.90, 0.9, 0.9)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)

	// Directly link a 4th, already-a-member drift face with a HIGHER score
	// than the 3 near faces -- simulating a stale member from some earlier
	// pass, before its embedding (or the person's template) drifted apart.
	// SelectExemplars' score-desc sort seeds farthest-point sampling from
	// this highest-score face first; with cap=3 that admits only 2 of the 3
	// identical near faces, leaving the third as a plain (non-exemplar)
	// member -- exactly the setup needed to observe the recompute.
	driftFaceID := insertAssetFaceQuality(t, db, "near-drift", driftVec, 0.99, 0.9, 0.9)
	linkMember(t, db, driftFaceID, pid, 0)

	// Second pass: step 1 selects exemplars from {near-a,b,c,drift} under
	// cap=3 -- drift (highest score) seeds the sample, admitting 2 of the 3
	// identical near faces and leaving the third out. Revalidation then
	// evaluates all 4 current members against those 3 exemplars: drift's
	// median distance is dominated by the 2 near exemplars sharing the pool
	// and gets detached; the near faces all stay (median 0 against their own
	// identical peers).
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, driftFaceID)
	if found {
		require.NotEqual(t, pid, gotPid, "the drift face must be detached from pid")
	}

	var exemplarCount, memberCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM face_person WHERE person_id=? AND exemplar=1`, pid).Scan(&exemplarCount))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM face_person WHERE person_id=?`, pid).Scan(&memberCount))

	require.Equal(t, 3, memberCount, "the 3 near faces must remain pid's only members after drift is detached")
	require.Equal(t, 3, exemplarCount,
		"all 3 remaining members must be exemplars after recompute -- a stale pre-removal exemplar set "+
			"(computed under cap pressure from the now-detached drift face) would have left one at exemplar=0")
}

// TestRunClustering_AppleRevalidate_SoloExemplarSelfMatchSurvives is the
// brief's required 1-exemplar-person self-match edge case: when a person has
// exactly one gated exemplar and that exemplar IS the member under
// revalidation, the member trivially matches itself (its own vector, at
// cosine distance 0, is its own nearest -- and only -- neighbor in the
// single-person pool), so it is never removed. Run across three passes to
// confirm this is stable, not a one-off accident of pass ordering.
func TestRunClustering_AppleRevalidate_SoloExemplarSelfMatchSurvives(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, _ := vecAtDist(dim, 0.05)
	soloFaceID := insertAssetFaceQuality(t, db, "solo", baseVec, 0.9, 0.9, 0.9)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoError(t, svc.RunClustering(context.Background()))

		gotPid, confirmed, found := membership(t, db, soloFaceID)
		require.True(t, found, "pass %d: the sole exemplar member must still exist", i)
		require.Equal(t, pid, gotPid, "pass %d: a 1-exemplar person's sole member must survive self-match revalidation", i)
		require.False(t, confirmed)

		var exemplarFlag int
		require.NoError(t, db.QueryRow(
			`SELECT exemplar FROM face_person WHERE face_id=?`, soloFaceID).Scan(&exemplarFlag))
		require.Equal(t, 1, exemplarFlag, "pass %d: the sole member must remain the person's exemplar", i)
	}
}

// TestRunClustering_AppleRevalidate_DbscanEngineUntouched pins the ENGINE
// SPLIT boundary: revalidation is an exemplar-era concept and must not run
// at all for the dbscan engine -- a member linked far from its person's
// legacy centroid is left in place, since dbscan has no per-pass
// revalidation mechanism (only the anchored centroid + assignEpsilon snap
// for FREE faces, entirely separate from already-anchored membership).
func TestRunClustering_AppleRevalidate_DbscanEngineUntouched(t *testing.T) {
	setClusterEngine(t, "dbscan")
	db := makeTestFaceDB(t)
	dim := 512

	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "dbscan-a1", normalize(a))
	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)

	far := make([]float32, dim)
	far[1] = 1.0 // orthogonal, cosine distance 1.0 -- far beyond any threshold
	farFaceID := insertAssetFace(t, db, "dbscan-far", normalize(far))
	linkMember(t, db, farFaceID, pid, 0)

	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, farFaceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "dbscan engine must never revalidate/detach an already-anchored member")
}
