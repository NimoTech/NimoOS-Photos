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
	"go.uber.org/zap"
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
			// Allow CORS preflight — TUS client sends OPTIONS before every request.
			if c.Request().Method == http.MethodOptions {
				return true
			}
			// Media serving endpoints: thumbnail/original/live are already
			// protected by the Gateway; <img> tags can't send Authorization headers.
			p := c.Path()
			if strings.HasSuffix(p, "/thumbnail") ||
				strings.HasSuffix(p, "/original") ||
				strings.HasSuffix(p, "/live") ||
				strings.HasSuffix(p, "/favorites/export") {
				return true
			}
			// Allow internal service calls from localhost (e.g. NimoOS-AI agent)
			// to POST /search/smart without a JWT.
			return strings.HasSuffix(p, "/search/smart") &&
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
	index := v1.NewIndexHandler(svc, "/DATA/Gallery")
	favorites := v1.NewFavoritesHandler(svc, "/DATA/Gallery", runtimePath)
	views := v1.NewViewsHandler(svc)

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

	// Album batch
	g.POST("/albums/:id/assets/batch", albums.BatchAdd)

	// Favorites
	g.POST("/favorites/:asset_id",   favorites.Favorite)
	g.DELETE("/favorites/:asset_id", favorites.Unfavorite)
	g.GET("/favorites/ids",          favorites.ListIDs)
	g.GET("/favorites",              favorites.List)
	g.GET("/favorites/export",       favorites.Export)
	g.GET("/favorites/top",          favorites.Top)

	// Asset views (open counter)
	g.POST("/views/:asset_id", views.Record)

	// Person endpoints
	g.GET("/persons", persons.List)
	g.PUT("/persons/:id", persons.UpdateName)
	g.GET("/persons/:id/assets", persons.Assets)
	g.POST("/persons/merge", persons.Merge)

	// Indexer status/control
	g.GET("/status", index.Status)
	g.POST("/scan", index.Scan)

	tasks := v1.NewTasksHandler(svc)
	g.GET("/tasks", tasks.List)

	cfg := v1.NewConfigHandler(svc)
	g.GET("/config", cfg.GetConfig)
	g.PUT("/config", cfg.UpdateConfig)

	// TUS resumable upload endpoints — register outside the v1 group so the
	// path is exactly /v1/upload-tus (matching the frontend tusClient base URL).
	tusH, err := v1.NewTUSHandler(svc, "/DATA/Gallery")
	if err != nil {
		zap.L().Fatal("failed to init TUS handler", zap.Error(err))
	}
	// TUS protocol uses POST/PATCH/HEAD/OPTIONS on the base path and child paths.
	e.Any("/v1/upload-tus", echo.WrapHandler(tusH))
	e.Any("/v1/upload-tus/*", echo.WrapHandler(tusH))

	return e
}
