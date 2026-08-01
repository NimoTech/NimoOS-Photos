package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

type placesStubServices struct {
	service.Services
	places *service.PlacesService
}

func (s placesStubServices) Places() *service.PlacesService { return s.places }

func newPlacesHarness(t *testing.T) (*PlacesHandler, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "places_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	gaz, err := geo.Load()
	if err != nil {
		t.Fatal(err)
	}
	geoSvc := service.NewGeoService(db, gaz)
	albums := service.NewAlbumService(db)
	placesSvc := service.NewPlacesServiceWithAlbums(db, gaz, geoSvc, albums)
	svc := placesStubServices{places: placesSvc}
	h := NewPlacesHandler(svc)
	return h, func() { db.Close() }
}

// TestPlacesListEmpty verifies that List returns 200 with a JSON stats field when the library is empty.
func TestPlacesListEmpty(t *testing.T) {
	h, cleanup := newPlacesHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/places", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	if err := h.List(c); err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp service.PlacesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v — body: %s", err, rec.Body.String())
	}
	// With an empty library, stats are all 0; places/regions may be nil or an empty slice.
	if resp.Stats.Cities < 0 {
		t.Errorf("unexpected negative cities: %d", resp.Stats.Cities)
	}
}

// TestPlacesGetNotFound verifies that a nonexistent key returns 404.
func TestPlacesGetNotFound(t *testing.T) {
	h, cleanup := newPlacesHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/places/9999", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("9999")

	err := h.Get(c)
	if err == nil {
		t.Fatal("expected HTTPError, got nil")
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T", err)
	}
	if httpErr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", httpErr.Code)
	}
}

// TestPlacesGetBadKey verifies that a non-integer key returns 400.
func TestPlacesGetBadKey(t *testing.T) {
	h, cleanup := newPlacesHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/places/abc", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("abc")

	err := h.Get(c)
	if err == nil {
		t.Fatal("expected HTTPError for bad key, got nil")
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T", err)
	}
	if httpErr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", httpErr.Code)
	}
}
