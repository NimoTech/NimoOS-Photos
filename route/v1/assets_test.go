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

// TestListAssetsHalfSpotLatLon verifies that supplying only one of spot_lat /
// spot_lon (without its counterpart) returns 400 with a message indicating the
// pair must be provided together.
func TestListAssetsHalfSpotLatLon(t *testing.T) {
	h := newAssetsHandler(t)
	e := echo.New()

	cases := []struct {
		name  string
		query string
	}{
		{"only spot_lat", "/v1/photos/assets?spot_key=1:10:20&spot_lat=35.0"},
		{"only spot_lon", "/v1/photos/assets?spot_key=1:10:20&spot_lon=139.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := h.List(c)
			if err == nil {
				t.Fatal("expected HTTPError for half spot_lat/spot_lon pair, got nil")
			}
			httpErr, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
			}
			if httpErr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", httpErr.Code)
			}
			msg, _ := httpErr.Message.(string)
			if msg != "spot_lat and spot_lon must be provided together" {
				t.Fatalf("unexpected error message: %q", msg)
			}
		})
	}
}

// TestListAssetsSpotLatLonOutOfRange verifies that spot_lat/spot_lon values
// outside the valid geographic ranges (lat ∉ [-90,90], lon ∉ [-180,180])
// return 400 with a message indicating the values are out of range.
func TestListAssetsSpotLatLonOutOfRange(t *testing.T) {
	h := newAssetsHandler(t)
	e := echo.New()

	cases := []struct {
		name  string
		query string
	}{
		{"lat out of range (91)", "/v1/photos/assets?spot_key=1:10:20&spot_lat=91&spot_lon=0"},
		{"lon out of range (181)", "/v1/photos/assets?spot_key=1:10:20&spot_lat=0&spot_lon=181"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := h.List(c)
			if err == nil {
				t.Fatal("expected HTTPError for out-of-range coordinates, got nil")
			}
			httpErr, ok := err.(*echo.HTTPError)
			if !ok {
				t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
			}
			if httpErr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", httpErr.Code)
			}
			msg, _ := httpErr.Message.(string)
			if msg != "spot_lat/spot_lon out of range" {
				t.Fatalf("unexpected error message: %q", msg)
			}
		})
	}
}
