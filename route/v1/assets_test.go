package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// assetsStubServices embeds service.Services (nil) so only the methods
// actually called by the tested code paths need to be implemented.
// For invalid-key tests the handler returns before calling Search(), so
// we do not need a real SearchService.
type assetsStubServices struct {
	placesStubServices // reuse the embedded service.Services nil stub
}

func newAssetsHandler(t *testing.T) *AssetsHandler {
	t.Helper()
	svc := assetsStubServices{}
	return NewAssetsHandler(svc, t.TempDir())
}

// TestListAssetsInvalidPlaceKey verifies that a non-numeric place_key returns 400.
func TestListAssetsInvalidPlaceKey(t *testing.T) {
	h := newAssetsHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/assets?place_key=abc", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.List(c)
	if err == nil {
		t.Fatal("expected HTTPError for invalid place_key, got nil")
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if httpErr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", httpErr.Code)
	}
}

// TestListAssetsInvalidSpotKey verifies that a malformed spot_key returns 400.
func TestListAssetsInvalidSpotKey(t *testing.T) {
	h := newAssetsHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/assets?spot_key=not-valid", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.List(c)
	if err == nil {
		t.Fatal("expected HTTPError for invalid spot_key, got nil")
	}
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if httpErr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", httpErr.Code)
	}
}
