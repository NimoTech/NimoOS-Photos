package route

import (
	"crypto/ecdsa"
	"net/http"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/NimoTech/NimoOS-Photos/common"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

// InitRouter sets up the Echo router with JWT middleware and all v1 routes.
func InitRouter(svc service.Services, runtimePath string, thumbDir string) http.Handler {
	e := echo.New()
	e.HideBanner = true

	e.Use(echo_middleware.Recover())
	e.Use(echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{echo.GET, echo.POST, echo.PUT, echo.DELETE, echo.OPTIONS},
		AllowHeaders: []string{echo.HeaderAuthorization, echo.HeaderContentType},
	}))

	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			// Allow internal service calls from localhost (e.g. NimoOS-AI agent)
			// to POST /search/smart without a JWT.
			return strings.HasSuffix(c.Path(), "/search/smart") &&
				strings.HasPrefix(c.RealIP(), "127.")
		},
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
				return external.GetPublicKey(runtimePath)
			})
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}
			c.Request().Header.Set("X-NimoOS-User-ID", strconv.Itoa(claims.ID))
			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				return []string{strings.TrimPrefix(auth, "Bearer ")}, nil
			},
		},
	}))

	// Health check — no auth required.
	e.GET("/healthz", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	g := e.Group(common.V1APIPath)

	assets := v1.NewAssetsHandler(svc, thumbDir)
	search := v1.NewSearchHandler(svc)
	tl := v1.NewTimelineHandler(svc)
	albums := v1.NewAlbumsHandler(svc)
	persons := v1.NewPersonsHandler(svc)
	index := v1.NewIndexHandler(svc)

	// Asset endpoints
	g.GET("/assets", assets.List)
	g.POST("/assets/upload", assets.Upload)
	g.GET("/assets/:id", assets.Get)
	g.DELETE("/assets/:id", assets.Delete)
	g.GET("/assets/:id/thumbnail", assets.Thumbnail)
	g.GET("/assets/:id/original", assets.Original)
	g.GET("/assets/:id/live", assets.Live)

	// Search endpoints
	g.POST("/search/smart", search.Smart)
	g.GET("/search/faces/:person_id", search.ByPerson)

	// Timeline
	g.GET("/timeline", tl.List)

	// Album endpoints
	g.GET("/albums", albums.List)
	g.POST("/albums", albums.Create)
	g.GET("/albums/:id", albums.Get)
	g.DELETE("/albums/:id", albums.Delete)
	g.POST("/albums/:id/assets", albums.AddAsset)
	g.DELETE("/albums/:id/assets/:asset", albums.RemoveAsset)

	// Person endpoints
	g.GET("/persons", persons.List)
	g.PUT("/persons/:id", persons.UpdateName)
	g.GET("/persons/:id/assets", persons.Assets)
	g.POST("/persons/merge", persons.Merge)

	// Indexer status/control
	g.GET("/status", index.Status)
	g.POST("/scan", index.Scan)

	return e
}
