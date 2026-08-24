package v1_test

// Route-level tests for Task 9 of the threshold self-calibration plan: the
// three calibration HTTP endpoints (status, history, factory-profile
// hot-update). Exercises real handlers dispatched through a real Echo
// router (registered in the exact order route/router.go uses) against a
// real (temp file) SQLite DB, so this also proves the
// "/persons/calibration*" vs "/persons/:id" route-order trap doesn't
// resurface here (same class of bug /persons/hidden hit).

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// newCalibTestEcho wires the three calibration endpoints plus GET
// /persons/:id, registered in the same relative order as route/router.go.
func newCalibTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "calib.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { service.SetCalibrationDB(nil) }) // service.go's NewService is the only other caller; reset for later tests.
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewPersonsHandler(svc, t.TempDir(), t.TempDir(), context.Background())
	g.GET("/persons/calibration", h.GetCalibration)
	g.GET("/persons/calibration/history", h.CalibrationHistory)
	g.PUT("/persons/calibration/profile", h.PutCalibrationProfile)
	g.GET("/persons/:id", h.Get)
	return e, db
}

// cInsertCalibState inserts a calibration_state row directly, as if a
// prior calibration run had already applied it.
func cInsertCalibState(t *testing.T, db *sql.DB, key string, value float64, modelGen string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO calibration_state(key, value, model_gen, updated_at) VALUES(?, ?, ?, ?)`,
		key, value, modelGen, time.Now().UTC())
	require.NoError(t, err)
}

// cInsertCalibHistory inserts one calibration_history row directly.
func cInsertCalibHistory(t *testing.T, db *sql.DB, tier, outcome string, runAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
		VALUES(?, ?, ?, '{}', '{}', '{}', ?)`, runAt, common.MLModelGen, tier, outcome)
	require.NoError(t, err)
}

// ── ① GET /persons/calibration ──────────────────────────────────────────

// TestGetCalibration_FiveKeysWithCorrectSource proves the response carries
// all five calibratable keys and that a key with a calibration_state row
// (matching the current model gen) reports source "calibrated" with that
// row's value, while the rest resolve from the stored (here: builtin
// factory) profile, source "profile" -- resolveThreshold's layer 4 "code"
// fallback only ever fires when no calibration DB is wired at all (see
// resolveBelowConf), which is not this test's setup.
func TestGetCalibration_FiveKeysWithCorrectSource(t *testing.T) {
	e, db := newCalibTestEcho(t)
	service.SetCalibrationDB(db)
	cInsertCalibState(t, db, "AssignAutoDist", 0.41, common.MLModelGen)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Thresholds []struct {
			Key       string  `json:"key"`
			Effective float64 `json:"effective"`
			Source    string  `json:"source"`
			ModelGen  string  `json:"modelGen"`
		} `json:"thresholds"`
		ProfileVersion int `json:"profileVersion"`
		Tiers          []struct {
			Tier string         `json:"tier"`
			Bars map[string]any `json:"bars"`
		} `json:"tiers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Thresholds, 5)
	wantKeys := map[string]bool{
		"AssignAutoDist": true, "AssignSuggestDist": true,
		"ClusterTightEps": true, "ClusterMergeEps": true, "MomentGapMinutes": true,
	}
	byKey := map[string]string{}
	for _, th := range body.Thresholds {
		require.True(t, wantKeys[th.Key], "unexpected key %q", th.Key)
		byKey[th.Key] = th.Source
		require.NotEmpty(t, th.ModelGen)
		if th.Key == "AssignAutoDist" {
			require.Equal(t, "calibrated", th.Source)
			require.InDelta(t, 0.41, th.Effective, 1e-9)
		} else {
			require.Equal(t, "profile", th.Source)
		}
	}
	require.Equal(t, 1, body.ProfileVersion)
	require.Len(t, body.Tiers, 3, "knn/merge/twopass")
}

// ── ② GET /persons/calibration/history ──────────────────────────────────

// TestCalibrationHistory_LimitAndDescendingOrder proves limit is honored and
// runs come back run_at DESC.
func TestCalibrationHistory_LimitAndDescendingOrder(t *testing.T) {
	e, db := newCalibTestEcho(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cInsertCalibHistory(t, db, "knn", "applied", base)
	cInsertCalibHistory(t, db, "merge", "held_insufficient", base.Add(1*time.Hour))
	cInsertCalibHistory(t, db, "twopass", "applied", base.Add(2*time.Hour))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration/history?limit=2", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Runs []struct {
			Tier    string `json:"tier"`
			Outcome string `json:"outcome"`
		} `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 2, "limit=2 must cap the result")
	require.Equal(t, "twopass", body.Runs[0].Tier, "most recent run_at first")
	require.Equal(t, "merge", body.Runs[1].Tier)
}

// TestCalibrationHistory_DefaultLimit proves the default limit is 50 when
// unspecified.
func TestCalibrationHistory_DefaultLimit(t *testing.T) {
	e, db := newCalibTestEcho(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		cInsertCalibHistory(t, db, "knn", "applied", base.Add(time.Duration(i)*time.Minute))
	}

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration/history", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Runs []map[string]any `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 50, "default limit must be 50")
}

// ── ③ PUT /persons/calibration/profile: valid update ────────────────────

// validProfileV2JSON returns a valid, version-2 profile document, with
// AssignAutoDist's default nudged so the GET-side effect is observable
// (Task's ③ requirement: PUT must move a key with no calibration_state row
// -- i.e. sourced from "profile" -- to the new default).
func validProfileV2JSON(t *testing.T) []byte {
	t.Helper()
	raw := `{
		"version": 2,
		"thresholds": {
			"AssignAutoDist":    {"default": 0.47, "min": 0.38, "max": 0.52},
			"AssignSuggestDist": {"default": 0.60, "min": 0.52, "max": 0.68},
			"ClusterTightEps":   {"default": 0.35, "min": 0.28, "max": 0.42},
			"ClusterMergeEps":   {"default": 0.55, "min": 0.48, "max": 0.65},
			"MomentGapMinutes":  {"default": 60, "min": 15, "max": 120}
		},
		"rules": {
			"maxStepDist": 0.02, "minDeltaDist": 0.01,
			"maxStepMinutes": 15, "minDeltaMinutes": 10,
			"cooldownHours": 24
		}
	}`
	return []byte(raw)
}

// TestPutCalibrationProfile_ValidUpdate_ChangesProfileVersionAndEffective
// proves a valid PUT returns 200 {"version": N}, and that a subsequent GET
// reflects the new profileVersion AND the new default for a key with no
// calibration_state row (source "profile").
func TestPutCalibrationProfile_ValidUpdate_ChangesProfileVersionAndEffective(t *testing.T) {
	e, db := newCalibTestEcho(t)
	service.SetCalibrationDB(db) // so GET's resolveThreshold actually consults the stored profile below.

	req := httptest.NewRequest(http.MethodPut, "/v1/photos/persons/calibration/profile", strings.NewReader(string(validProfileV2JSON(t))))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var putResp struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putResp))
	require.Equal(t, 2, putResp.Version)

	// ── ⑤ cache invalidation: GET immediately after must reflect the new profile.
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration", nil))
	require.Equal(t, http.StatusOK, rec2.Code)

	var body struct {
		Thresholds []struct {
			Key       string  `json:"key"`
			Effective float64 `json:"effective"`
			Source    string  `json:"source"`
		} `json:"thresholds"`
		ProfileVersion int `json:"profileVersion"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &body))
	require.Equal(t, 2, body.ProfileVersion, "GET must reflect the just-stored profile version")

	found := false
	for _, th := range body.Thresholds {
		if th.Key == "AssignAutoDist" {
			found = true
			require.Equal(t, "profile", th.Source, "no calibration_state row for this key -- must resolve from the stored profile")
			require.InDelta(t, 0.47, th.Effective, 1e-9, "must reflect the new profile default, not the old code default (0.45)")
		}
	}
	require.True(t, found)
}

// ── ④ PUT /persons/calibration/profile: non-increasing version rejected ──

// TestPutCalibrationProfile_NonIncreasingVersion_Returns400 proves a PUT
// whose version does not exceed the current stored/implicit version 400s
// with a {"message": "..."} body, and never touches the stored profile.
func TestPutCalibrationProfile_NonIncreasingVersion_Returns400(t *testing.T) {
	e, _ := newCalibTestEcho(t)

	badRaw := `{
		"version": 1,
		"thresholds": {
			"AssignAutoDist":    {"default": 0.45, "min": 0.38, "max": 0.52},
			"AssignSuggestDist": {"default": 0.60, "min": 0.52, "max": 0.68},
			"ClusterTightEps":   {"default": 0.35, "min": 0.28, "max": 0.42},
			"ClusterMergeEps":   {"default": 0.55, "min": 0.48, "max": 0.65},
			"MomentGapMinutes":  {"default": 60, "min": 15, "max": 120}
		},
		"rules": {
			"maxStepDist": 0.02, "minDeltaDist": 0.01,
			"maxStepMinutes": 15, "minDeltaMinutes": 10,
			"cooldownHours": 24
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/v1/photos/persons/calibration/profile", strings.NewReader(badRaw))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var body struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Message)

	// Confirm the GET-side profileVersion is untouched (still the implicit builtin 1).
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration", nil))
	var getBody struct {
		ProfileVersion int `json:"profileVersion"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &getBody))
	require.Equal(t, 1, getBody.ProfileVersion)
}

// ── ⑥ route order: /persons/calibration must not be swallowed by :id ────

// TestCalibrationRoute_NotSwallowedByID proves GET /persons/calibration
// reaches GetCalibration, not Get with id="calibration" (which would 404
// since no person literally named "calibration" exists) -- the same class
// of bug /persons/hidden and /persons/suggestions hit.
func TestCalibrationRoute_NotSwallowedByID(t *testing.T) {
	e, db := newCalibTestEcho(t)
	_, err := db.Exec(`INSERT INTO persons(id) VALUES('real-1')`)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body, "thresholds", "must reach GetCalibration, not Get(\"calibration\")")

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/calibration/history", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	var body2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &body2))
	require.Contains(t, body2, "runs", "must reach CalibrationHistory, not Get(\"calibration\")")

	rec3 := httptest.NewRecorder()
	e.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/real-1", nil))
	require.Equal(t, http.StatusOK, rec3.Code)
	var body3 map[string]any
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &body3))
	require.Contains(t, body3, "person", "GET /persons/:id must still dispatch to Get for a real id")
}
