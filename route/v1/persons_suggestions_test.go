package v1_test

// Route-level tests for Task 6 of the exemplar-assignment plan: the
// person-suggestions API (GET list + accept/reject/batch). Exercises real
// handlers dispatched through a real Echo router (registered in the exact
// order route/router.go uses) against a real (temp file) SQLite DB, so this
// also proves the "/persons/suggestions" vs "/persons/:id" route-order trap
// (same class of bug /persons/hidden hit — see persons_hidden_route_test.go)
// doesn't resurface here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// newSuggestionsTestEcho wires the four new endpoints plus GET /persons/:id,
// registered in the same relative order as route/router.go.
func newSuggestionsTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sugg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewPersonsHandler(svc, t.TempDir(), t.TempDir(), context.Background())
	g.GET("/persons/suggestions", h.PersonSuggestions)
	g.POST("/persons/suggestions/batch", h.BatchPersonSuggestions)
	g.POST("/persons/suggestions/:id/accept", h.AcceptPersonSuggestion)
	g.POST("/persons/suggestions/:id/reject", h.RejectPersonSuggestion)
	g.GET("/persons/:id", h.Get)
	return e, db
}

// ── fixture helpers ─────────────────────────────────────────────────────

func sInsertAsset(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES(?, ?, ?, 'indexed')`,
		assetID, "/x/"+assetID, "chk-"+assetID)
	require.NoError(t, err)
}

func sInsertFace(t *testing.T, db *sql.DB, faceID, assetID string) {
	t.Helper()
	vec := sqlite.SerializeFloat32([]float32{1, 0, 0})
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?, ?, '{}', ?)`,
		faceID, assetID, vec)
	require.NoError(t, err)
}

func sInsertPerson(t *testing.T, db *sql.DB, personID string, hidden bool) {
	t.Helper()
	h := 0
	if hidden {
		h = 1
	}
	_, err := db.Exec(`INSERT INTO persons(id, hidden) VALUES(?, ?)`, personID, h)
	require.NoError(t, err)
}

func sInsertMember(t *testing.T, db *sql.DB, faceID, personID string, confirmed bool) {
	t.Helper()
	c := 0
	if confirmed {
		c = 1
	}
	_, err := db.Exec(`INSERT INTO face_person(face_id, person_id, confirmed) VALUES(?, ?, ?)`, faceID, personID, c)
	require.NoError(t, err)
}

// sMarkExemplar flags an existing face_person row (inserted via
// sInsertMember) as one of its person's quality-gated exemplar templates.
func sMarkExemplar(t *testing.T, db *sql.DB, faceID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE face_person SET exemplar=1 WHERE face_id=?`, faceID)
	require.NoError(t, err)
}

// sSetFaceScore sets a face_detections row's detector quality score, used to
// exercise the exemplar strip's quality ordering. A nil score leaves the
// column at its default NULL, exercising the NULLS-LAST tie case.
func sSetFaceScore(t *testing.T, db *sql.DB, faceID string, score float64) {
	t.Helper()
	_, err := db.Exec(`UPDATE face_detections SET score=? WHERE id=?`, score, faceID)
	require.NoError(t, err)
}

func sInsertSuggestion(t *testing.T, db *sql.DB, id, personID, faceID, kind string, score float64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		id, personID, faceID, kind, score, time.Now())
	require.NoError(t, err)
}

func sSuggestionStatus(t *testing.T, db *sql.DB, id string) (status string, decidedAt sql.NullTime) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT status, decided_at FROM person_suggestions WHERE id=?`, id).Scan(&status, &decidedAt))
	return
}

func sMember(t *testing.T, db *sql.DB, faceID string) (personID string, confirmed bool, found bool) {
	t.Helper()
	var pid string
	var c int
	err := db.QueryRow(`SELECT person_id, confirmed FROM face_person WHERE face_id=?`, faceID).Scan(&pid, &c)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	require.NoError(t, err)
	return pid, c != 0, true
}

func sNegativeExists(t *testing.T, db *sql.DB, personID, faceID string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM person_negatives WHERE person_id=? AND face_id=?`, personID, faceID).Scan(&n))
	return n == 1
}

// ── GET /persons/suggestions ────────────────────────────────────────────

// TestPersonSuggestions_ListGroupedAndSorted proves the list groups by
// person, embeds the full Person object, includes faceId/assetId/kind/
// score/createdAt per suggestion, and sorts suggestions within a group by
// score ascending.
func TestPersonSuggestions_ListGroupedAndSorted(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)

	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	sInsertFace(t, db, "f1", "a1")
	sInsertFace(t, db, "f2", "a2")
	sInsertPerson(t, db, "p1", false)
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id='p1'`)
	require.NoError(t, err)
	// Higher score inserted first, lower score second -- list must still
	// come back sorted ascending regardless of insertion order.
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.58)
	sInsertSuggestion(t, db, "s2", "p1", "f2", "join", 0.30)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Groups []struct {
			Person struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"person"`
			Suggestions []struct {
				ID        string  `json:"id"`
				FaceID    string  `json:"faceId"`
				AssetID   string  `json:"assetId"`
				Kind      string  `json:"kind"`
				Score     float64 `json:"score"`
				CreatedAt string  `json:"createdAt"`
			} `json:"suggestions"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Groups, 1)
	require.Equal(t, "p1", body.Groups[0].Person.ID)
	require.Equal(t, "Alice", body.Groups[0].Person.Name)
	require.Len(t, body.Groups[0].Suggestions, 2)
	require.Equal(t, "s2", body.Groups[0].Suggestions[0].ID, "lower score (0.30) must sort first")
	require.Equal(t, "f2", body.Groups[0].Suggestions[0].FaceID)
	require.Equal(t, "a2", body.Groups[0].Suggestions[0].AssetID)
	require.Equal(t, "join", body.Groups[0].Suggestions[0].Kind)
	require.NotEmpty(t, body.Groups[0].Suggestions[0].CreatedAt)
	require.Equal(t, "s1", body.Groups[0].Suggestions[1].ID)
}

// TestPersonSuggestions_ExemplarFaceIDs_OrderedAndCapped proves each group
// carries at most 5 of the person's exemplar faces (face_person.exemplar=1),
// ordered by detector quality score descending -- the review wizard's header
// reference strip needs its best faces first and must never grow past the
// cap even when a person has many exemplars.
func TestPersonSuggestions_ExemplarFaceIDs_OrderedAndCapped(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertPerson(t, db, "p1", false)

	// Six exemplars with distinct descending scores -- only the best 5 may
	// come back, in score-descending order.
	scores := []float64{0.9, 0.8, 0.7, 0.6, 0.5, 0.4}
	ids := make([]string, len(scores))
	for i, sc := range scores {
		fid := fmt.Sprintf("ex%d", i)
		ids[i] = fid
		sInsertFace(t, db, fid, "a1")
		sInsertMember(t, db, fid, "p1", true)
		sMarkExemplar(t, db, fid)
		sSetFaceScore(t, db, fid, sc)
	}
	// The suggestion-triggering face itself, unrelated to the exemplar set.
	sInsertFace(t, db, "sf", "a1")
	sInsertSuggestion(t, db, "s1", "p1", "sf", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Groups []struct {
			ExemplarFaceIDs []string `json:"exemplarFaceIds"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Groups, 1)
	require.Equal(t, ids[:5], body.Groups[0].ExemplarFaceIDs,
		"must return the top 5 by score descending, capped at 5")
}

// TestPersonSuggestions_ExemplarFaceIDs_ExcludesCoverAndNullsLast proves the
// person's cover_face_id is excluded from the exemplar list (the frontend
// renders the cover separately and would otherwise show a duplicate tile),
// and that an exemplar with no detector score (NULL) sorts after every
// scored exemplar rather than before or in an unspecified position.
func TestPersonSuggestions_ExemplarFaceIDs_ExcludesCoverAndNullsLast(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertPerson(t, db, "p1", false)

	sInsertFace(t, db, "ex0", "a1")
	sInsertMember(t, db, "ex0", "p1", true)
	sMarkExemplar(t, db, "ex0")
	sSetFaceScore(t, db, "ex0", 0.9)

	sInsertFace(t, db, "ex1", "a1")
	sInsertMember(t, db, "ex1", "p1", true)
	sMarkExemplar(t, db, "ex1")
	sSetFaceScore(t, db, "ex1", 0.5)

	// No score set -- stays NULL, must sort last.
	sInsertFace(t, db, "ex-null", "a1")
	sInsertMember(t, db, "ex-null", "p1", true)
	sMarkExemplar(t, db, "ex-null")

	// Highest-scoring exemplar of all, but it's the person's cover face --
	// must never appear in the list.
	sInsertFace(t, db, "cover-f", "a1")
	sInsertMember(t, db, "cover-f", "p1", true)
	sMarkExemplar(t, db, "cover-f")
	sSetFaceScore(t, db, "cover-f", 0.99)
	_, err := db.Exec(`UPDATE persons SET cover_face_id='cover-f' WHERE id='p1'`)
	require.NoError(t, err)

	sInsertFace(t, db, "sf", "a1")
	sInsertSuggestion(t, db, "s1", "p1", "sf", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Groups []struct {
			ExemplarFaceIDs []string `json:"exemplarFaceIds"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Groups, 1)
	require.Equal(t, []string{"ex0", "ex1", "ex-null"}, body.Groups[0].ExemplarFaceIDs,
		"cover face excluded; NULL-score exemplar must sort after all scored ones")
}

// TestPersonSuggestions_ExemplarFaceIDs_EmptyWhenNoExemplars proves a person
// with open suggestions but zero exemplar-flagged faces gets back an empty
// JSON array ("exemplarFaceIds":[]), never a bare absence of the field and
// never JSON null -- the frontend iterates the field directly with no
// null-guard.
func TestPersonSuggestions_ExemplarFaceIDs_EmptyWhenNoExemplars(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"exemplarFaceIds":[]`,
		"must serialize as an empty array, not null and not an absent field")

	var body struct {
		Groups []struct {
			ExemplarFaceIDs []string `json:"exemplarFaceIds"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Groups, 1)
	require.NotNil(t, body.Groups[0].ExemplarFaceIDs)
	require.Empty(t, body.Groups[0].ExemplarFaceIDs)
}

// TestPersonSuggestions_ExcludesClosedAndOtherPersons proves accepted/
// rejected suggestions and suggestions for other persons never leak into a
// group they don't belong to.
func TestPersonSuggestions_ExcludesClosedAndOtherPersons(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)
	_, err := db.Exec(`UPDATE person_suggestions SET status='accepted', decided_at=? WHERE id='s1'`, time.Now())
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body["groups"])
}

// TestPersonSuggestions_HiddenPersonInvisible proves a hidden person's open
// suggestion never appears in the list.
func TestPersonSuggestions_HiddenPersonInvisible(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "hp", true)
	sInsertSuggestion(t, db, "s1", "hp", "f1", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body["groups"])
}

// TestPersonSuggestionsRoute_NotSwallowedByID proves GET /persons/suggestions
// reaches PersonSuggestions, not Get with id="suggestions" (which would 404
// since no person literally named "suggestions" exists) — the same class of
// bug /persons/hidden hit.
func TestPersonSuggestionsRoute_NotSwallowedByID(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertPerson(t, db, "real-1", false)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/suggestions", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body, "groups", "must reach PersonSuggestions, not Get(\"suggestions\")")

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/real-1", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	var body2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &body2))
	require.Contains(t, body2, "person", "GET /persons/:id must still dispatch to Get for a real id")
}

// ── POST /persons/suggestions/:id/accept ────────────────────────────────

// TestAcceptSuggestion_Join proves accepting a 'join' suggestion (the face
// was never a member) inserts a confirmed=1 face_person row, marks the
// suggestion accepted, and recomputes the person's centroid (a non-null
// centroid is the observable proxy for recomputeOneCentroidTx having run).
func TestAcceptSuggestion_Join(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var dec struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		DecidedAt string `json:"decidedAt"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dec))
	require.Equal(t, "s1", dec.ID)
	require.Equal(t, "accepted", dec.Status)
	require.NotEmpty(t, dec.DecidedAt)

	pid, confirmed, found := sMember(t, db, "f1")
	require.True(t, found)
	require.Equal(t, "p1", pid)
	require.True(t, confirmed)

	status, decidedAt := sSuggestionStatus(t, db, "s1")
	require.Equal(t, "accepted", status)
	require.True(t, decidedAt.Valid)

	var centroid []byte
	require.NoError(t, db.QueryRow(`SELECT centroid FROM persons WHERE id='p1'`).Scan(&centroid))
	require.NotEmpty(t, centroid, "recomputeOneCentroidTx must have run and written a centroid")
}

// TestAcceptSuggestion_JoinAlreadyMember covers the brief's explicit
// already-auto-assigned-meanwhile case: the face is already a face_person
// member of the SAME person (confirmed=0) by the time accept runs (e.g. a
// concurrent clustering pass auto-joined it). Accept must just flip
// confirmed=1, not error or duplicate the row.
func TestAcceptSuggestion_JoinAlreadyMember(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	pid, confirmed, found := sMember(t, db, "f1")
	require.True(t, found)
	require.Equal(t, "p1", pid)
	require.True(t, confirmed)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE face_id='f1'`).Scan(&n))
	require.Equal(t, 1, n, "must not duplicate the face_person row")
}

// TestAcceptSuggestion_Review proves accepting a 'review' suggestion (an
// existing unconfirmed member drifting into the gray zone) flips its
// confirmed flag to 1 without touching membership.
func TestAcceptSuggestion_Review(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "review", 0.5)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	pid, confirmed, found := sMember(t, db, "f1")
	require.True(t, found)
	require.Equal(t, "p1", pid)
	require.True(t, confirmed)
	status, _ := sSuggestionStatus(t, db, "s1")
	require.Equal(t, "accepted", status)
}

// TestAcceptSuggestion_JoinMovesFaceFromDifferentPersonAndRecomputesOldPerson
// covers the brief's other explicit meanwhile-elsewhere case: the face was
// auto-assigned to a DIFFERENT person (pB) by a clustering pass that ran
// between the suggestion being created (for pA) and the user's accept
// arriving. An explicit accept must win: the face moves to pA (confirmed=1),
// and pB -- now down a member -- gets its stats recomputed rather than left
// pointing at a stale count/centroid.
func TestAcceptSuggestion_JoinMovesFaceFromDifferentPersonAndRecomputesOldPerson(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "pA", false)
	sInsertPerson(t, db, "pB", false)
	// f1 is currently a member of pB (e.g. auto-joined there by a clustering
	// pass), while an open 'join' suggestion proposes pA for the same face.
	sInsertMember(t, db, "f1", "pB", false)
	sInsertSuggestion(t, db, "s1", "pA", "f1", "join", 0.4)

	var pBCountBefore int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id='pB'`).Scan(&pBCountBefore))
	require.Equal(t, 1, pBCountBefore)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	pid, confirmed, found := sMember(t, db, "f1")
	require.True(t, found)
	require.Equal(t, "pA", pid, "an explicit accept must win and move the face to the suggested person")
	require.True(t, confirmed)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE face_id='f1'`).Scan(&n))
	require.Equal(t, 1, n, "the face must not end up a member of both persons")

	var pBCountAfter int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id='pB'`).Scan(&pBCountAfter))
	require.Equal(t, 0, pBCountAfter, "pB's member count must drop once its face moved to pA")

	var pACentroid, pBCentroid []byte
	require.NoError(t, db.QueryRow(`SELECT centroid FROM persons WHERE id='pA'`).Scan(&pACentroid))
	require.NoError(t, db.QueryRow(`SELECT centroid FROM persons WHERE id='pB'`).Scan(&pBCentroid))
	require.NotEmpty(t, pACentroid, "pA's centroid must be recomputed with its new member")
	require.Empty(t, pBCentroid, "pB's centroid must be cleared once it has no members left")
}

// TestAcceptSuggestion_NotFound proves accepting an unknown suggestion id 404s.
func TestAcceptSuggestion_NotFound(t *testing.T) {
	e, _ := newSuggestionsTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/no-such-id/accept", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestAcceptSuggestion_Idempotent proves a repeat accept on an already-
// decided suggestion is a no-op that returns the current (accepted) state
// with 200, not a 409 and not a second write.
func TestAcceptSuggestion_Idempotent(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec1.Code)
	var dec1 struct {
		Status    string `json:"status"`
		DecidedAt string `json:"decidedAt"`
	}
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &dec1))

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusOK, rec2.Code, "repeat decision must be a 200 no-op, not 409")
	var dec2 struct {
		Status    string `json:"status"`
		DecidedAt string `json:"decidedAt"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &dec2))
	require.Equal(t, "accepted", dec2.Status)
	require.Equal(t, dec1.DecidedAt, dec2.DecidedAt, "the no-op path must not overwrite decided_at")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE face_id='f1'`).Scan(&n))
	require.Equal(t, 1, n)
}

// ── POST /persons/suggestions/:id/reject ────────────────────────────────

// TestRejectSuggestion_Join proves rejecting a 'join' suggestion (the face
// was never a member) records a person_negatives row and marks the
// suggestion rejected, without ever creating a face_person row.
func TestRejectSuggestion_Join(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/reject", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var dec struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dec))
	require.Equal(t, "rejected", dec.Status)

	status, decidedAt := sSuggestionStatus(t, db, "s1")
	require.Equal(t, "rejected", status)
	require.True(t, decidedAt.Valid)
	require.True(t, sNegativeExists(t, db, "p1", "f1"))

	_, _, found := sMember(t, db, "f1")
	require.False(t, found, "a rejected 'join' suggestion must never create a membership")
}

// TestRejectSuggestion_Review proves rejecting a 'review' suggestion detaches
// the existing membership (DELETE face_person) in addition to recording the
// negative and marking the suggestion rejected.
func TestRejectSuggestion_Review(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertMember(t, db, "f1", "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "review", 0.5)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/reject", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	_, _, found := sMember(t, db, "f1")
	require.False(t, found, "reject on a 'review' suggestion must delete the face_person row")
	require.True(t, sNegativeExists(t, db, "p1", "f1"))
	status, _ := sSuggestionStatus(t, db, "s1")
	require.Equal(t, "rejected", status)
}

// TestRejectSuggestion_NotFound proves rejecting an unknown suggestion id 404s.
func TestRejectSuggestion_NotFound(t *testing.T) {
	e, _ := newSuggestionsTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/no-such-id/reject", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestRejectSuggestion_Idempotent proves a repeat reject is a no-op 200
// returning the already-rejected state, not a second negatives insert error.
func TestRejectSuggestion_Idempotent(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s1", "p1", "f1", "join", 0.4)

	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/reject", nil))
	require.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/reject", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	var dec2 struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &dec2))
	require.Equal(t, "rejected", dec2.Status)
}

// ── hidden-person suggestions: invisible AND inoperable ─────────────────

// TestHiddenPersonSuggestion_Inoperable proves a hidden person's suggestion
// 404s on both accept and reject, even though the row genuinely exists.
func TestHiddenPersonSuggestion_Inoperable(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertFace(t, db, "f1", "a1")
	sInsertPerson(t, db, "hp", true)
	sInsertSuggestion(t, db, "s1", "hp", "f1", "join", 0.4)

	recA := httptest.NewRecorder()
	e.ServeHTTP(recA, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/accept", nil))
	require.Equal(t, http.StatusNotFound, recA.Code)

	recR := httptest.NewRecorder()
	e.ServeHTTP(recR, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/s1/reject", nil))
	require.Equal(t, http.StatusNotFound, recR.Code)

	status, _ := sSuggestionStatus(t, db, "s1")
	require.Equal(t, "open", status, "a 404'd operation must not have decided the suggestion")
}

// ── POST /persons/suggestions/batch ──────────────────────────────────────

// TestBatchPersonSuggestions_MixedOutcomes proves the batch endpoint always
// returns 200 with a per-id result map covering accepted, rejected, and
// error (unknown id) outcomes in the same request.
func TestBatchPersonSuggestions_MixedOutcomes(t *testing.T) {
	e, db := newSuggestionsTestEcho(t)
	sInsertAsset(t, db, "a1")
	sInsertAsset(t, db, "a2")
	sInsertFace(t, db, "f1", "a1")
	sInsertFace(t, db, "f2", "a2")
	sInsertPerson(t, db, "p1", false)
	sInsertSuggestion(t, db, "s-accept", "p1", "f1", "join", 0.4)
	sInsertSuggestion(t, db, "s-reject", "p1", "f2", "join", 0.5)

	body := `{"accept":["s-accept","s-missing"],"reject":["s-reject"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/persons/suggestions/batch", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Results map[string]struct {
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "accepted", resp.Results["s-accept"].Status)
	require.Equal(t, "rejected", resp.Results["s-reject"].Status)
	require.Equal(t, "error", resp.Results["s-missing"].Status)
	require.NotEmpty(t, resp.Results["s-missing"].Error)

	sAccStatus, _ := sSuggestionStatus(t, db, "s-accept")
	require.Equal(t, "accepted", sAccStatus)
	sRejStatus, _ := sSuggestionStatus(t, db, "s-reject")
	require.Equal(t, "rejected", sRejStatus)
}
