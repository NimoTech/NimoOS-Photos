package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"
)

func TestMcpReadSkip(t *testing.T) {
	base := common.V1APIPath
	cases := []struct {
		name                     string
		method, path, ip, userID string
		want                     bool
	}{
		{"albums GET localhost with uid", http.MethodGet, base + "/albums", "127.0.0.1", "42", true},
		{"albums GET localhost no uid (fail-closed)", http.MethodGet, base + "/albums", "127.0.0.1", "", false},
		{"albums POST localhost (write stays protected)", http.MethodPost, base + "/albums", "127.0.0.1", "42", false},
		{"albums GET external ip", http.MethodGet, base + "/albums", "192.168.1.5", "42", false},
		{"search/smart POST localhost with uid", http.MethodPost, base + "/search/smart", "127.0.0.1", "42", true},
		{"suffix-lookalike not exact-matched", http.MethodGet, base + "/public/albums", "127.0.0.1", "42", false},
	}
	for _, c := range cases {
		if got := mcpReadSkip(c.method, c.path, c.ip, c.userID); got != c.want {
			t.Errorf("%s: mcpReadSkip=%v want %v", c.name, got, c.want)
		}
	}
}

// TestMediaGetSkip is the unit test for mediaGetSkip: the thumbnail/preview/sprite
// suffix whitelist must be GET-only; a POST hitting the same suffix (e.g.
// /smart-views/preview) must not be let through.
func TestMediaGetSkip(t *testing.T) {
	base := common.V1APIPath
	cases := []struct {
		name         string
		method, path string
		want         bool
	}{
		{"GET assets/:id/preview allowed", http.MethodGet, base + "/assets/:id/preview", true},
		{"GET assets/:id/sprite allowed", http.MethodGet, base + "/assets/:id/sprite", true},
		{"GET assets/:id/thumbnail allowed", http.MethodGet, base + "/assets/:id/thumbnail", true},
		{"GET favorites/export allowed", http.MethodGet, base + "/favorites/export", true},
		{"GET albums/:id/export allowed", http.MethodGet, base + "/albums/:id/export", true},
		{"POST albums/:id/export NOT allowed (method pre-check)", http.MethodPost, base + "/albums/:id/export", false},
		{"GET smart-views/:id/export allowed", http.MethodGet, base + "/smart-views/:id/export", true},
		{"POST smart-views/:id/export NOT allowed (existing POST route must not be wrongly let through)", http.MethodPost, base + "/smart-views/:id/export", false},
		{"POST smart-views/preview NOT allowed (collision regression)", http.MethodPost, base + "/smart-views/preview", false},
		{"POST assets/:id/preview (if present) NOT allowed", http.MethodPost, base + "/assets/:id/preview", false},
		{"PUT assets/:id/sprite (if present) NOT allowed", http.MethodPut, base + "/assets/:id/sprite", false},
	}
	for _, c := range cases {
		if got := mediaGetSkip(c.method, c.path); got != c.want {
			t.Errorf("%s: mediaGetSkip=%v want %v", c.name, got, c.want)
		}
	}
}

// newTestJWTRouter builds a minimal Echo instance whose JWT middleware is
// structurally identical to InitRouter's (the same Skipper logic:
// mediaGetSkip + mcpReadSkip), and registers routes with the same production
// path prefix that carry the "POST /smart-views/preview vs GET */preview"
// suffix-collision risk — used to reproduce/regress the auth-bypass issue
// reported by the reviewer at the real HTTP request layer (not just by
// calling the bare functions). ParseTokenFunc always fails, simulating a
// request with "no/invalid JWT" — as long as the Skipper doesn't wrongly
// let it through, the middleware must return 401.
func newTestJWTRouter() *echo.Echo {
	e := echo.New()
	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			if c.Request().Method == http.MethodOptions {
				return true
			}
			p := c.Path()
			if p == "/healthz" || p == common.V1APIPath+"/version" {
				return true
			}
			if mediaGetSkip(c.Request().Method, p) {
				return true
			}
			return mcpReadSkip(c.Request().Method, p, c.RealIP(),
				c.Request().Header.Get("X-NimoOS-User-ID"))
		},
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			return nil, echo.ErrUnauthorized
		},
		// Kept consistent with the production config (router.go): the custom
		// extractor only does TrimPrefix, returning an empty string when the
		// Authorization header is missing rather than erroring, leaving
		// ParseTokenFunc to fail it. If this were omitted, Echo would fall back
		// to its default header extractor, which short-circuits with 400 (instead
		// of the 401 actually seen in production) when the header is missing,
		// masking the real behavior.
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				return []string{strings.TrimPrefix(auth, "Bearer ")}, nil
			},
		},
	}))

	ok := func(c echo.Context) error { return c.String(http.StatusOK, "ok") }

	e.GET("/healthz", ok)

	g := e.Group(common.V1APIPath)
	g.POST("/smart-views/preview", ok)
	g.GET("/assets/:id/preview", ok)
	g.GET("/assets/:id/sprite", ok)
	return e
}

// TestJWTExemption_PreviewSuffixCollision is a route-layer regression test:
//  1. POST /smart-views/preview without an Authorization header must be
//     rejected by the JWT middleware (the auth bypass reproduced by the
//     reviewer: before the fix, this request would be wrongly let through by
//     the "/preview" suffix whitelist and return 200; this case FAILs before
//     the fix and PASSes after).
//  2. GET /assets/:id/preview and GET /assets/:id/sprite without an
//     Authorization header should still be let through to the handler as
//     normal (the media exemption must not be broken).
func TestJWTExemption_PreviewSuffixCollision(t *testing.T) {
	e := newTestJWTRouter()

	post := httptest.NewRequest(http.MethodPost, common.V1APIPath+"/smart-views/preview", nil)
	postRec := httptest.NewRecorder()
	e.ServeHTTP(postRec, post)
	require.Equal(t, http.StatusUnauthorized, postRec.Code,
		"POST /smart-views/preview must not be wrongly let through by the /preview suffix whitelist (auth bypass regression)")

	getPreview := httptest.NewRequest(http.MethodGet, common.V1APIPath+"/assets/abc/preview", nil)
	getPreviewRec := httptest.NewRecorder()
	e.ServeHTTP(getPreviewRec, getPreview)
	require.Equal(t, http.StatusOK, getPreviewRec.Code,
		"GET /assets/:id/preview should remain JWT-exempt and reach the handler normally")

	getSprite := httptest.NewRequest(http.MethodGet, common.V1APIPath+"/assets/abc/sprite", nil)
	getSpriteRec := httptest.NewRecorder()
	e.ServeHTTP(getSpriteRec, getSprite)
	require.Equal(t, http.StatusOK, getSpriteRec.Code,
		"GET /assets/:id/sprite should remain JWT-exempt and reach the handler normally")
}

// TestJWTExemption_Healthz is a route-layer regression test for the /healthz
// endpoint: router.go registers it with a comment claiming "no auth
// required", but the JWT Skipper never actually exempted it — a request
// without an Authorization header was rejected with 401 in production. This
// test reproduces that at the real HTTP request layer (not just by calling
// the Skipper helpers) and must pass once /healthz is added to the Skipper's
// exempt set alongside /version.
func TestJWTExemption_Healthz(t *testing.T) {
	e := newTestJWTRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"GET /healthz without a token must be JWT-exempt as the router.go comment claims")
}
