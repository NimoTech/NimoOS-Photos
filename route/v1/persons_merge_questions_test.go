package v1_test

// Route-level tests for the cluster-merge questions API (GET
// /persons/merge-suggestions/v2 + accept/reject), registered through a real
// Echo router in the same relative order as route/router.go, against a real
// (temp file) SQLite DB. Reuses sInsertAsset/sInsertPerson/sInsertMember from
// persons_suggestions_test.go (same package).

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// newMergeQuestionsTestEcho wires the three new endpoints plus GET
// /persons/:id, registered in the same relative order route/router.go uses.
func newMergeQuestionsTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "mergeq.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewPersonsHandler(svc, t.TempDir(), t.TempDir(), context.Background())
	g.GET("/persons/merge-suggestions/v2", h.MergeQuestions)
	g.POST("/persons/merge-suggestions/v2/:id/accept", h.AcceptMergeQuestion)
	g.POST("/persons/merge-suggestions/v2/:id/reject", h.RejectMergeQuestion)
	g.GET("/persons/:id", h.Get)
	return e, db
}

// mqVecAtCosineDistance mirrors service's vecAtCosineDistance (unexported,
// same package-private helper shape) since this test lives in the v1_test
// package and cannot import it directly.
func mqVecAtCosineDistance(dim int, dist float64) (v1, v2 []float32) {
	v1 = make([]float32, dim)
	v1[0] = 1.0
	cosTheta := 1.0 - dist
	sinTheta := math.Sqrt(1 - cosTheta*cosTheta)
	v2 = make([]float32, dim)
	v2[0] = float32(cosTheta)
	v2[1] = float32(sinTheta)
	return v1, v2
}

func mqInsertFaceWithVec(t *testing.T, db *sql.DB, faceID, assetID string, vec []float32) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?, ?, '{}', ?)`,
		faceID, assetID, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

// mqInsertMergeSuggestion inserts a row in the canonical (person_a<person_b,
// into_is_a) shape the merge_suggestions table now stores -- mirrors
// service.mergeSuggestionDirection's logic locally, since that helper is
// unexported and this file lives in the v1_test package.
func mqInsertMergeSuggestion(t *testing.T, db *sql.DB, id, fromID, intoID string, dist float64) {
	t.Helper()
	var personA, personB string
	var intoIsA bool
	if fromID < intoID {
		personA, personB, intoIsA = fromID, intoID, false // into=intoID=personB
	} else {
		personA, personB, intoIsA = intoID, fromID, true // into=intoID=personA
	}
	_, err := db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES(?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`, id, personA, personB, intoIsA, dist)
	require.NoError(t, err)
}

func mqSuggestionStatus(t *testing.T, db *sql.DB, id string) (status string, decidedAt sql.NullTime) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT status, decided_at FROM merge_suggestions WHERE id=?`, id).Scan(&status, &decidedAt))
	return
}

// ── GET /persons/merge-suggestions/v2 ──────────────────────────────────────

// mqFaceRef mirrors service.PreviewFace's JSON shape ({faceId, assetId}) for
// decoding in this package.
type mqFaceRef struct {
	FaceID  string `json:"faceId"`
	AssetID string `json:"assetId"`
}

// TestMergeQuestions_ListShapeAndOrder proves the response shape ({pairs:
// [...]}), embeds the full from/into Person objects, includes up-to-4
// preview face ids per side, and orders by dist ascending.
func TestMergeQuestions_ListShapeAndOrder(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	sInsertAsset(t, db, "a3")
	sInsertAsset(t, db, "a4")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	vC, vD := mqVecAtCosineDistance(8, 0.60)
	mqInsertFaceWithVec(t, db, "f1", "a1", vA)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	mqInsertFaceWithVec(t, db, "f3", "a3", vC)
	mqInsertFaceWithVec(t, db, "f4", "a4", vD)
	sInsertPerson(t, db, "p1", false)
	sInsertPerson(t, db, "p2", false)
	sInsertPerson(t, db, "p3", false)
	sInsertPerson(t, db, "p4", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertMember(t, db, "f2", "p2", false)
	sInsertMember(t, db, "f3", "p3", false)
	sInsertMember(t, db, "f4", "p4", false)
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id='p1'`)
	require.NoError(t, err)

	// Closer pair (dist 0.60) inserted first, farther-among-the-two (0.58 is
	// actually closer) second -- list must come back sorted by dist
	// ascending regardless of insertion order.
	mqInsertMergeSuggestion(t, db, "m-far", "p3", "p4", 0.60)
	mqInsertMergeSuggestion(t, db, "m-near", "p2", "p1", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/merge-suggestions/v2", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Pairs []struct {
			ID   string  `json:"id"`
			Dist float64 `json:"dist"`
			From struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"from"`
			Into struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"into"`
			FromFaceIDs []string    `json:"fromFaceIds"`
			IntoFaceIDs []string    `json:"intoFaceIds"`
			FromFaces   []mqFaceRef `json:"fromFaces"`
			IntoFaces   []mqFaceRef `json:"intoFaces"`
		} `json:"pairs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Pairs, 2)
	require.Equal(t, "m-near", body.Pairs[0].ID, "dist ascending: 0.58 before 0.60")
	require.Equal(t, "m-far", body.Pairs[1].ID)
	require.Equal(t, "p2", body.Pairs[0].From.ID)
	require.Equal(t, "p1", body.Pairs[0].Into.ID)
	require.Equal(t, "Alice", body.Pairs[0].Into.Name)
	require.Equal(t, []string{"f2"}, body.Pairs[0].FromFaceIDs)
	require.Equal(t, []string{"f1"}, body.Pairs[0].IntoFaceIDs)

	// fromFaces/intoFaces must be the same faces, same order, as
	// fromFaceIds/intoFaceIds, each additionally carrying the asset id the
	// face belongs to (f2 -> a2, f1 -> a1) so the merge-card UI can open the
	// full photo behind a preview face.
	require.Equal(t, []mqFaceRef{{FaceID: "f2", AssetID: "a2"}}, body.Pairs[0].FromFaces)
	require.Equal(t, []mqFaceRef{{FaceID: "f1", AssetID: "a1"}}, body.Pairs[0].IntoFaces)
}

// TestMergeQuestions_FromFacesEmptyArrayNotNull proves a side with no active
// member faces (e.g. a person with zero face_person rows) reports both its
// preview arrays as [] rather than null.
func TestMergeQuestions_FromFacesEmptyArrayNotNull(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a2")
	_, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	sInsertPerson(t, db, "p1", false) // no members at all
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "f2", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p1", "p2", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/merge-suggestions/v2", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Pairs []struct {
			ID          string          `json:"id"`
			FromFaceIDs json.RawMessage `json:"fromFaceIds"`
			FromFaces   json.RawMessage `json:"fromFaces"`
		} `json:"pairs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Pairs, 1)
	require.JSONEq(t, "[]", string(body.Pairs[0].FromFaceIDs))
	require.JSONEq(t, "[]", string(body.Pairs[0].FromFaces))
}

// TestMergeQuestions_HiddenPersonExcluded: a pair where either side is
// hidden must not appear in the list.
func TestMergeQuestions_HiddenPersonExcluded(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "f1", "a1", vA)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	sInsertPerson(t, db, "p1", true) // hidden
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertMember(t, db, "f2", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p2", "p1", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/merge-suggestions/v2", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Pairs []map[string]any `json:"pairs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body.Pairs, "a pair with a hidden person on either side must be excluded")
}

// ── POST /persons/merge-suggestions/v2/:id/accept ──────────────────────────

// TestAcceptMergeQuestion_MergesPersonsAndMarksAccepted proves accept merges
// the two persons (member counts move) via the real merge machinery and
// marks the row accepted.
func TestAcceptMergeQuestion_MergesPersonsAndMarksAccepted(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "f1", "a1", vA)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	sInsertPerson(t, db, "p1", false)
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertMember(t, db, "f2", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p1", "p2", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/accept", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var one int
	err := db.QueryRow(`SELECT 1 FROM persons WHERE id='p1'`).Scan(&one)
	require.Equal(t, sql.ErrNoRows, err, "the 'from' person must have been deleted by the merge")

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id='p2'`).Scan(&cnt))
	require.Equal(t, 2, cnt, "the 'into' person must now own both faces")

	status, decidedAt := mqSuggestionStatus(t, db, "m1")
	require.Equal(t, "accepted", status)
	require.True(t, decidedAt.Valid)
}

// TestAcceptMergeQuestion_UnknownIDNotFound: an unknown id 404s.
func TestAcceptMergeQuestion_UnknownIDNotFound(t *testing.T) {
	e, _ := newMergeQuestionsTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/nope/accept", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAcceptMergeQuestion_DeadPersonIDRow404sAndDeletesRow: an open row whose
// person ids no longer exist in persons (simulating a stale row generation's
// own per-pass cleanup hasn't caught yet) must 404 on accept AND the
// now-useless row must be deleted eagerly, not left open for a repeat
// attempt to hit the same dead id again, and not marked 'rejected' (no
// human decision was actually made).
func TestAcceptMergeQuestion_DeadPersonIDRow404sAndDeletesRow(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	mqInsertMergeSuggestion(t, db, "m1", "dead-from", "dead-into", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/accept", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions WHERE id='m1'`).Scan(&cnt))
	require.Equal(t, 0, cnt, "the dead-id row must be deleted, not left open or marked rejected")
}

// TestAcceptMergeQuestion_RepeatCallIsNoOp: accepting an already-decided row
// again 200s with its current state instead of re-merging or erroring.
func TestAcceptMergeQuestion_RepeatCallIsNoOp(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "f1", "a1", vA)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	sInsertPerson(t, db, "p1", false)
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertMember(t, db, "f2", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p1", "p2", 0.58)

	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/accept", nil))
	require.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/accept", nil))
	require.Equal(t, http.StatusOK, rec2.Code, "a repeat accept must not error (from person is already gone)")

	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &body))
	require.Equal(t, "accepted", body.Status)
}

// ── POST /persons/merge-suggestions/v2/:id/reject ──────────────────────────

// TestRejectMergeQuestion_WritesNormalizedFacePairAndSuppressesRegeneration
// proves reject writes a normalized (face_a<face_b) face_negative_pairs row
// for the two clusters' representative faces, and that a subsequent
// generation pass for the exact same pair is suppressed by it.
func TestRejectMergeQuestion_WritesNormalizedFacePairAndSuppressesRegeneration(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "fz", "a1", vA)
	mqInsertFaceWithVec(t, db, "fa", "a2", vB)
	sInsertPerson(t, db, "p1", false)
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "fz", "p1", false)
	sInsertMember(t, db, "fa", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p1", "p2", 0.58)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/reject", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	status, decidedAt := mqSuggestionStatus(t, db, "m1")
	require.Equal(t, "rejected", status)
	require.True(t, decidedAt.Valid)

	// "fa" < "fz" lexicographically -- must be stored (face_a, face_b) = (fa, fz).
	var faceA, faceB string
	require.NoError(t, db.QueryRow(`SELECT face_a, face_b FROM face_negative_pairs`).Scan(&faceA, &faceB))
	require.Equal(t, "fa", faceA)
	require.Equal(t, "fz", faceB)

	// Both persons still exist (reject never merges) and the pair must still
	// be far enough apart to still qualify as gray-band, to prove
	// suppression -- not distance filtering -- is what blocks regeneration.
	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id IN ('p1','p2')`).Scan(&cnt))
	require.Equal(t, 2, cnt)
}

// TestRejectMergeQuestion_RepeatCallIsNoOp: rejecting an already-decided row
// again 200s with its current state and does not write a second negative pair.
func TestRejectMergeQuestion_RepeatCallIsNoOp(t *testing.T) {
	e, db := newMergeQuestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	vA, vB := mqVecAtCosineDistance(8, 0.58)
	mqInsertFaceWithVec(t, db, "f1", "a1", vA)
	mqInsertFaceWithVec(t, db, "f2", "a2", vB)
	sInsertPerson(t, db, "p1", false)
	sInsertPerson(t, db, "p2", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertMember(t, db, "f2", "p2", false)
	mqInsertMergeSuggestion(t, db, "m1", "p1", "p2", 0.58)

	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/reject", nil))
	require.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/m1/reject", nil))
	require.Equal(t, http.StatusOK, rec2.Code)

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_negative_pairs`).Scan(&cnt))
	require.Equal(t, 1, cnt, "a repeat reject must not write a second negative pair")
}

// TestRejectMergeQuestion_UnknownIDNotFound: an unknown id 404s.
func TestRejectMergeQuestion_UnknownIDNotFound(t *testing.T) {
	e, _ := newMergeQuestionsTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/merge-suggestions/v2/nope/reject", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
