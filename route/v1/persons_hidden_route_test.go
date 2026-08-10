package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
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

// newPersonsHiddenTestEcho registers the persons routes in the exact same
// order as route/router.go — GET "/persons/hidden" before GET
// "/persons/:id" — using the real handlers and a real (temp) SQLite DB, so
// this exercises actual route dispatch instead of assuming Echo's
// static-vs-param precedence.
func newPersonsHiddenTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ph.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewPersonsHandler(svc, t.TempDir(), t.TempDir(), context.Background())
	g.GET("/persons/hidden", h.ListHidden)
	g.GET("/persons/:id", h.Get)
	g.POST("/persons/:id/hide", h.Hide)
	return e, db
}

// TestPersonsRouteOrder_HiddenNotSwallowedByID proves GET /persons/hidden
// reaches the hidden-list handler (not Get with id="hidden"), and that GET
// /persons/:id still dispatches to Get normally for a real person id.
func TestPersonsRouteOrder_HiddenNotSwallowedByID(t *testing.T) {
	e, db := newPersonsHiddenTestEcho(t)

	// A real, visible person — proves the :id route still resolves normally.
	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES('real-1', 'Real Person')`)
	require.NoError(t, err)

	// GET /persons/hidden must reach ListHidden, not Get("hidden") (which
	// would 404 since no person with id "hidden" exists).
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/hidden", nil))
	require.Equal(t, http.StatusOK, rec.Code,
		"GET /persons/hidden must not be swallowed as GET /persons/:id with id=hidden (would 404)")
	var hiddenList []service.Person
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hiddenList))
	require.Empty(t, hiddenList)

	// GET /persons/real-1 must still reach Get and return the {person, relations} shape.
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/real-1", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &body))
	require.Contains(t, body, "person", "GET /persons/:id must still reach Get, not ListHidden")
	require.Contains(t, body, "relations")

	// POST /persons/real-1/hide, then confirm it now surfaces via GET /persons/hidden.
	rec3 := httptest.NewRecorder()
	e.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/real-1/hide", nil))
	require.Equal(t, http.StatusOK, rec3.Code)

	rec4 := httptest.NewRecorder()
	e.ServeHTTP(rec4, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/hidden", nil))
	require.Equal(t, http.StatusOK, rec4.Code)
	var hiddenList2 []service.Person
	require.NoError(t, json.Unmarshal(rec4.Body.Bytes(), &hiddenList2))
	require.Len(t, hiddenList2, 1)
	require.Equal(t, "real-1", hiddenList2[0].ID)

	// And GET /persons/real-1 (now hidden) must 404 through Get — proving the
	// two routes reach genuinely distinct handlers with distinct semantics.
	rec5 := httptest.NewRecorder()
	e.ServeHTTP(rec5, httptest.NewRequest(http.MethodGet, "/v1/photos/persons/real-1", nil))
	require.Equal(t, http.StatusNotFound, rec5.Code, "hidden persons are not visible via GET /persons/:id")
}

// TestPersonsHide_NotFound proves POST /persons/:id/hide 404s for a missing person.
func TestPersonsHide_NotFound(t *testing.T) {
	e, _ := newPersonsHiddenTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/photos/persons/no-such-id/hide", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
