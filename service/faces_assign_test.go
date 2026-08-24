package service_test

// Tests for Task 4 of the exemplar-assignment plan: wiring exemplar-based
// KNN matching into rebuildPersonsWithProgress's step 1 (anchor building) and
// step 3 (free-face assignment) for the apple engine, replacing the old
// centroid+assignEpsilon snap -- which the dbscan engine keeps unchanged.
// Reuses makeTestFaceDB/insertAssetFace/normalize (faces_test.go) and
// setClusterEngine/personOfFace (faces_engine_test.go), same package.

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// vecAtDist returns two unit vectors of the given dimensionality whose
// cosine distance is exactly `dist` (cosDist = 1 - cosTheta): the same
// e0/e1-plane rotation trick as service.vecAtCosineDistance in
// cluster_engine_test.go, duplicated here because that helper is unexported
// and lives in the internal `service` package, not `service_test`. The first
// return value is always exactly e0 regardless of dist, so callers needing a
// single fixed base vector plus several faces at different distances from it
// can just take v1 from any one call.
func vecAtDist(dim int, dist float64) (v1, v2 []float32) {
	v1 = make([]float32, dim)
	v1[0] = 1.0

	cosTheta := 1.0 - dist
	sinTheta := math.Sqrt(1 - cosTheta*cosTheta)
	v2 = make([]float32, dim)
	v2[0] = float32(cosTheta)
	v2[1] = float32(sinTheta)
	return v1, v2
}

// insertAssetFaceQuality is insertAssetFace plus explicit
// score/frontality/sharpness on the face_detections row, needed to clear
// exemplarQualityGate()'s hard gate (NULL always fails it).
func insertAssetFaceQuality(t *testing.T, db *sql.DB, assetID string, vec []float32, score, frontality, sharpness float64) string {
	t.Helper()
	faceID := insertAssetFace(t, db, assetID, vec)
	_, err := db.Exec(`UPDATE face_detections SET score=?, frontality=?, sharpness=? WHERE id=?`,
		score, frontality, sharpness, faceID)
	require.NoError(t, err)
	return faceID
}

// setupAnchoredExemplarPerson builds one hidden (anchored) person with 3
// quality-gated member faces all at baseVec, so SelectExemplars gates all 3
// in as exemplars (well under the default cap of 24) and BuildExemplarIndex
// has enough votes available to clear the default minVotes=3 floor. Returns
// the person id.
func setupAnchoredExemplarPerson(t *testing.T, db *sql.DB, baseVec []float32) string {
	t.Helper()
	insertAssetFaceQuality(t, db, "seed-1", baseVec, 0.9, 0.9, 0.9)
	insertAssetFaceQuality(t, db, "seed-2", baseVec, 0.9, 0.9, 0.9)
	insertAssetFaceQuality(t, db, "seed-3", baseVec, 0.9, 0.9, 0.9)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)
	return pid
}

// suggestionRow reports whether an open 'join' suggestion exists for
// (personID, faceID), and its stored score.
func suggestionRow(t *testing.T, db *sql.DB, personID, faceID string) (exists bool, status string, score float64) {
	t.Helper()
	row := db.QueryRow(`SELECT status, score FROM person_suggestions WHERE person_id=? AND face_id=? AND kind='join'`,
		personID, faceID)
	err := row.Scan(&status, &score)
	if err == sql.ErrNoRows {
		return false, "", 0
	}
	require.NoError(t, err)
	return true, status, score
}

// membership reports the person_id (if any) a face currently belongs to, and
// whether it's confirmed.
func membership(t *testing.T, db *sql.DB, faceID string) (personID string, confirmed bool, found bool) {
	t.Helper()
	var pid sql.NullString
	var conf int
	err := db.QueryRow(`SELECT person_id, confirmed FROM face_person WHERE face_id=?`, faceID).Scan(&pid, &conf)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	require.NoError(t, err)
	return pid.String, conf != 0, true
}

// TestRunClustering_AppleAssign_AutoJoinsPersonUnconfirmed is brief case ①:
// a free face well within assignAutoDist of an anchored person's exemplars
// (and clearing assignMinVotes) is inserted directly into face_person with
// confirmed=0 -- not merely suggested.
func TestRunClustering_AppleAssign_AutoJoinsPersonUnconfirmed(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, autoVec := vecAtDist(dim, 0.05) // well under the default 0.45 autoDist
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "free-auto", autoVec)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, faceID)
	require.True(t, found, "an auto-decided face must have a face_person row")
	require.Equal(t, pid, gotPid, "the face must join the matched person")
	require.False(t, confirmed, "auto-joined faces must not be pre-confirmed")

	exists, _, _ := suggestionRow(t, db, pid, faceID)
	require.False(t, exists, "an auto decision must not also leave a suggestion behind")
}

// TestRunClustering_AppleAssign_OneExemplarPersonAutoJoins is the required
// wiring-level (end-to-end RunClustering) regression for the small-person-
// starvation fix in matcher.go's Match: an anchored person with exactly ONE
// quality-gated exemplar -- a freshly-hidden/named person with a single
// photo, an everyday real case -- must still auto-absorb a nearby free face.
// Pre-fix this was impossible under the flat minVotes=3 default (1 vote can
// never reach 3), a real regression against the old centroid snap that this
// same scenario used to handle fine. See matcher_test.go's
// TestMatchOneExemplarPersonAutoJoinsWithoutCompetitor for the pure-function
// version of this same case.
func TestRunClustering_AppleAssign_OneExemplarPersonAutoJoins(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, autoVec := vecAtDist(dim, 0.05) // well under the default 0.45 autoDist
	insertAssetFaceQuality(t, db, "seed-solo", baseVec, 0.9, 0.9, 0.9)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)

	faceID := insertAssetFace(t, db, "free-auto-solo", autoVec)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, faceID)
	require.True(t, found, "an auto-decided face must have a face_person row")
	require.Equal(t, pid, gotPid, "a one-exemplar person must still auto-absorb a close free face")
	require.False(t, confirmed, "auto-joined faces must not be pre-confirmed")
}

// TestRunClustering_AppleAssign_GrayZoneOpenSuggestionNotMember is brief
// case ②: a free face whose median distance lands strictly between
// assignAutoDist and assignSuggestDist produces an open 'join' suggestion
// for the matched person, without making the face a member of that person.
func TestRunClustering_AppleAssign_GrayZoneOpenSuggestionNotMember(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, grayVec := vecAtDist(dim, 0.5) // between the default 0.45/0.60 bounds
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "free-gray", grayVec)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, faceID)
	require.True(t, found, "a suggested face still lands somewhere via two-pass clustering")
	require.NotEqual(t, pid, gotPid, "a gray-zone face must not join the suggested person")

	exists, status, score := suggestionRow(t, db, pid, faceID)
	require.True(t, exists, "expected an open join suggestion")
	require.Equal(t, "open", status)
	require.Greater(t, score, 0.0)
}

// TestRunClustering_AppleAssign_NegatedPairNeitherAutoNorSuggest is brief
// case ③: a face at auto-distance from a person, but explicitly negated for
// that (person, face) pair beforehand, must neither join nor be suggested.
func TestRunClustering_AppleAssign_NegatedPairNeitherAutoNorSuggest(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, autoVec := vecAtDist(dim, 0.05)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "free-negated", autoVec)
	_, err := db.Exec(`INSERT INTO person_negatives(person_id, face_id, created_at) VALUES(?,?,?)`,
		pid, faceID, time.Now())
	require.NoError(t, err)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, _, found := membership(t, db, faceID)
	if found {
		require.NotEqual(t, pid, gotPid, "a negated pair must never join, even at auto distance")
	}

	exists, _, _ := suggestionRow(t, db, pid, faceID)
	require.False(t, exists, "a negated pair must never produce a suggestion either")
}

// TestRunClustering_EngineSplit_QualityGateOnlyAppliesToApple is brief case
// ④: with a free face whose quality signals are NULL (fails the apple
// engine's exemplar gate entirely), "dbscan" still snaps it onto the hidden
// anchored person via the untouched legacy centroid+assignEpsilon path,
// while "apple" cannot -- the anchored person has zero exemplars (nothing
// passed the gate), so BuildExemplarIndex's entry for it is empty and Match
// always returns "none" for every free face. This pins the engine-split
// boundary the brief calls out: dbscan's rollback path must be a complete,
// independent stack, not a partial mix with the exemplar matcher.
func TestRunClustering_EngineSplit_QualityGateOnlyAppliesToApple(t *testing.T) {
	for _, tc := range []struct {
		engine    string
		nearJoins bool
	}{
		{engine: "dbscan", nearJoins: true},
		{engine: "apple", nearJoins: false},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			setClusterEngine(t, tc.engine)
			db := makeTestFaceDB(t)
			dim := 512
			a := make([]float32, dim)
			a[0] = 1.0
			// No quality signals set (plain insertAssetFace) -- this would
			// fail the apple engine's exemplar gate outright.
			insertAssetFace(t, db, "hp-1", normalize(a))
			svc := service.NewFaceService(db)
			require.NoError(t, svc.RunClustering(context.Background()))

			var pid string
			require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
			_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
			require.NoError(t, err)

			a2 := make([]float32, dim)
			a2[0] = 0.97
			a2[1] = 0.03
			faceID := insertAssetFace(t, db, "hp-2", normalize(a2))
			require.NoError(t, svc.RunClustering(context.Background()))

			gotPid, _, found := membership(t, db, faceID)
			if tc.nearJoins {
				require.True(t, found)
				require.Equal(t, pid, gotPid, "engine=%s: the legacy centroid snap must still absorb the nearby face", tc.engine)
			} else {
				if found {
					require.NotEqual(t, pid, gotPid,
						"engine=%s: with zero gated exemplars the hidden person cannot absorb any face via matching", tc.engine)
				}
			}
		})
	}
}

// TestRunClustering_AppleAssign_SuggestionIdempotentAcrossPasses is brief
// case ⑤: running clustering twice with the same gray-zone face present must
// not create a second open suggestion row for the same (person, face) pair.
func TestRunClustering_AppleAssign_SuggestionIdempotentAcrossPasses(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, grayVec := vecAtDist(dim, 0.5)
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "free-gray", grayVec)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))
	require.NoError(t, svc.RunClustering(context.Background()))

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM person_suggestions WHERE person_id=? AND face_id=? AND kind='join'`,
		pid, faceID).Scan(&count))
	require.Equal(t, 1, count, "a second clustering pass must not duplicate the open suggestion")
}

// TestRunClustering_AppleAssign_AutoJoinResolvesStaleOpenJoinSuggestion is the
// IMPORTANT fix from the final whole-span review: a free face that carries a
// stale open 'join' suggestion for a person (queued by some earlier pass,
// back when it only cleared the gray zone) must have that row resolved
// (deleted) the moment it auto-joins that same person directly. Left open,
// the row would surface as a moot suggestion card; if a human rejects it
// later, decideSuggestion would write a person_negatives row for a face that
// is simultaneously a confirmed member of that person -- which a later
// revalidation pass would then silently evict via Match's negation filter
// stripping the person's own exemplars out of that face's pool.
func TestRunClustering_AppleAssign_AutoJoinResolvesStaleOpenJoinSuggestion(t *testing.T) {
	setClusterEngine(t, "apple")
	db := makeTestFaceDB(t)
	dim := 512

	baseVec, autoVec := vecAtDist(dim, 0.05) // well under the default 0.45 autoDist
	pid := setupAnchoredExemplarPerson(t, db, baseVec)

	faceID := insertAssetFace(t, db, "free-auto-stale-suggestion", autoVec)
	// Simulates a stale row left behind by an earlier pass (e.g. before this
	// person had enough gated exemplars, or before this face's own signal
	// improved) -- the face itself is, THIS pass, squarely within the auto
	// band, not the gray zone.
	_, err := db.Exec(`
		INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at)
		VALUES(?, ?, ?, 'join', ?, 'open', ?)`,
		uuid.NewString(), pid, faceID, 0.55, time.Now())
	require.NoError(t, err)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	gotPid, confirmed, found := membership(t, db, faceID)
	require.True(t, found)
	require.Equal(t, pid, gotPid, "the face must auto-join directly")
	require.False(t, confirmed, "auto-joined faces must not be pre-confirmed")

	exists, _, _ := suggestionRow(t, db, pid, faceID)
	require.False(t, exists, "the now-moot open join row must be resolved (deleted), not left dangling")
}
