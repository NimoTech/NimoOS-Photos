package route

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/middleware"
	commonUpload "github.com/NimoTech/NimoOS-Common/upload"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/NimoTech/NimoOS-Photos/common"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/NimoTech/NimoOS-Photos/service/uploadstore"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
)

// mediaGetSkip reports whether a request may skip JWT because it targets a
// read-only media-serving endpoint that <img>/<video> tags load without an
// Authorization header (thumbnail/face-thumbnail/original/live/sprite/
// preview/favorites/export). These are matched by PATH SUFFIX ONLY (Echo's
// c.Path() is the route pattern, e.g. "/v1/photos/assets/:id/preview", and
// does not encode the HTTP method), so this check MUST be GET-only:
// method 前置校验不可省略 —— 否则会连带放行同后缀的写接口，例如
// POST /v1/photos/smart-views/preview 会被误判命中 "/preview" 后缀而整体
// 绕过 JWT 鉴权（复现过：无 Authorization 头的该 POST 请求返回 200）。
// 媒体豁免名单里注册的路由全部是 GET，因此这里显式加 GET 前置条件兜底。
func mediaGetSkip(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	return strings.HasSuffix(path, "/thumbnail") ||
		strings.HasSuffix(path, "/face-thumbnail") ||
		strings.HasSuffix(path, "/original") ||
		strings.HasSuffix(path, "/live") ||
		strings.HasSuffix(path, "/sprite") ||
		strings.HasSuffix(path, "/preview") ||
		strings.HasSuffix(path, "/favorites/export")
}

// mcpReadSkip reports whether a localhost caller may skip JWT on the read-only
// photos endpoints the NimoOS-AI MCP server uses. Fail-closed + exact-match:
//   - realIP must be 127.* (use c.RealIP(); the Gateway strips spoofed XFF, so
//     external traffic never appears as 127, and RemoteAddr would be wrong here);
//   - userID (X-NimoOS-User-ID) must be non-empty, else NOT skipped → JWT → 401
//     (never fall back to the "default" user on the MCP path);
//   - path is matched EXACTLY against the full route (never HasSuffix).
func mcpReadSkip(method, path, realIP, userID string) bool {
	if !strings.HasPrefix(realIP, "127.") || userID == "" {
		return false
	}
	if method == http.MethodPost && path == common.V1APIPath+"/search/smart" {
		return true
	}
	if method == http.MethodGet && path == common.V1APIPath+"/albums" {
		return true
	}
	return false
}

// InitRouter sets up the Echo router with JWT middleware and all v1 routes.
func InitRouter(ctx context.Context, svc service.Services, runtimePath string, thumbDir string) http.Handler {
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
			if p == common.V1APIPath+"/version" {
				return true
			}
			if mediaGetSkip(c.Request().Method, p) {
				return true
			}
			// Allow localhost internal callers (NimoOS-AI MCP server) to skip JWT on
			// the read-only photos endpoints; fail-closed + exact-match (mcpReadSkip).
			return mcpReadSkip(c.Request().Method, c.Path(), c.RealIP(),
				c.Request().Header.Get("X-NimoOS-User-ID"))
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

	middleware.RegisterVersionRoute(e, common.V1APIPath+"/version", "Photos", common.PhotosVersion)

	assets := v1.NewAssetsHandler(svc, thumbDir)
	search := v1.NewSearchHandler(svc)
	tl := v1.NewTimelineHandler(svc)
	albums := v1.NewAlbumsHandler(svc)
	faceThumbDir := filepath.Join(filepath.Dir(thumbDir), "face-thumbs")
	persons := v1.NewPersonsHandler(svc, faceThumbDir, thumbDir, ctx)
	index := v1.NewIndexHandler(svc, "/DATA/Gallery")
	favorites := v1.NewFavoritesHandler(svc, "/DATA/Gallery", runtimePath)
	trash := v1.NewTrashHandler(svc)
	views := v1.NewViewsHandler(svc)

	// Asset endpoints
	g.GET("/assets", assets.List)
	g.POST("/assets/upload", assets.Upload)
	g.GET("/assets/:id", assets.Get)
	g.DELETE("/assets/:id", assets.Delete)
	g.GET("/assets/:id/thumbnail", assets.Thumbnail)
	g.GET("/assets/:id/original", assets.Original)
	g.GET("/assets/:id/live", assets.Live)
	g.GET("/assets/:id/sprite", assets.Sprite)
	g.GET("/assets/:id/preview", assets.Preview)
	g.GET("/assets/:id/ocr", assets.OCRLines)

	// Search endpoints
	g.POST("/search/smart", search.Smart)
	g.GET("/search/faces/:person_id", search.ByPerson)

	// Timeline
	g.GET("/timeline", tl.List)

	// Album endpoints
	g.GET("/albums", albums.List)
	g.POST("/albums", albums.Create)
	g.GET("/albums/:id", albums.Get)
	g.GET("/albums/:id/summary", albums.Summary)
	g.DELETE("/albums/:id", albums.Delete)
	g.POST("/albums/:id/assets", albums.AddAsset)
	g.DELETE("/albums/:id/assets/:asset", albums.RemoveAsset)

	// Album batch
	g.POST("/albums/:id/assets/batch", albums.BatchAdd)
	g.PATCH("/albums/:id", albums.Update)
	g.PATCH("/albums/:id/assets/order", albums.Reorder)

	// Places
	places := v1.NewPlacesHandler(svc)
	g.GET("/places", places.List)
	g.GET("/places/:key", places.Get)
	g.GET("/places/:key/cover-candidates", places.CoverCandidates)
	g.PUT("/places/:key/cover", places.SetCover)
	g.DELETE("/places/:key/cover", places.ResetCover)
	g.PUT("/places/:key/spot-name", places.SetSpotName)
	g.DELETE("/places/:key/spot-name", places.ResetSpotName)
	g.POST("/places/:key/album", places.CreateAlbum)

	// Favorites
	g.POST("/favorites/:asset_id", favorites.Favorite)
	g.DELETE("/favorites/:asset_id", favorites.Unfavorite)
	g.GET("/favorites/ids", favorites.ListIDs)
	g.GET("/favorites", favorites.List)
	g.GET("/favorites/export", favorites.Export)
	g.GET("/favorites/top", favorites.Top)

	// Trash（回收站）
	g.GET("/trash", trash.List)
	g.POST("/trash/restore", trash.RestoreBatch)
	g.POST("/trash/empty", trash.Empty)
	g.POST("/trash/:id/restore", trash.Restore)
	g.DELETE("/trash/:id", trash.Purge)

	// Asset views (open counter)
	g.POST("/views/:asset_id", views.Record)

	// Person endpoints
	g.GET("/persons", persons.List)
	g.GET("/persons/merge-suggestions", persons.MergeSuggestions)
	g.POST("/persons/merge-suggestions/reject", persons.RejectSuggestion)
	g.POST("/persons/merge", persons.Merge)
	g.POST("/persons/recluster", persons.Recluster)
	g.GET("/persons/:id", persons.Get)
	g.PUT("/persons/:id", persons.Update)
	g.DELETE("/persons/:id", persons.Delete)
	g.POST("/persons/:id/restore", persons.Restore)
	g.GET("/persons/:id/assets", persons.Assets)
	g.GET("/persons/:id/relations", persons.Relations)
	g.GET("/persons/:id/places", persons.Places)
	g.GET("/persons/:id/face-thumbnail", persons.FaceThumbnail)
	g.POST("/persons/:id/detach", persons.Detach)
	g.PUT("/persons/:id/cover", persons.SetCover)
	g.DELETE("/persons/:id/cover", persons.DeleteCover)

	// Indexer status/control
	g.GET("/status", index.Status)
	g.POST("/scan", index.Scan)

	tasks := v1.NewTasksHandler(svc)
	g.GET("/tasks", tasks.List)

	cfg := v1.NewConfigHandler(svc)
	g.GET("/config", cfg.GetConfig)
	g.PUT("/config", cfg.UpdateConfig)

	storage := v1.NewStorageHandler(svc)
	g.GET("/storage", storage.Get)
	g.POST("/cache/prune", storage.Prune)

	rebuild := v1.NewRebuildHandler(svc)
	g.POST("/index/rebuild", rebuild.Rebuild)

	about := v1.NewAboutHandler(svc)
	g.GET("/about", about.Get)

	smartViews := v1.NewSmartViewsHandler(svc)
	v1.RegisterSmartViewRoutes(g, smartViews)

	// 构造 upload Store(连接 photos.db),供 TUS handler 与 uploads API 共用。
	uploadStore := uploadstore.NewStore(svc.DB())

	// 注册 uploads 列出/详情/取消接口。
	uploadTasks := v1.NewUploadTasksHandler(uploadStore)
	g.GET("/uploads", uploadTasks.ListUploads)
	g.GET("/uploads/:id", uploadTasks.GetUpload)
	g.POST("/uploads/:id/cancel", uploadTasks.CancelUpload)

	// 启动分级 GC(后台 goroutine)。
	commonUpload.StartGC(uploadStore, commonUpload.GCConfig{
		StagingDir:     common.StagingDir,
		PausedTTL:      commonUpload.DefaultPausedTTLSeconds,
		GCIntervalSecs: commonUpload.DefaultGCIntervalSeconds,
	})

	// TUS resumable upload endpoints — register outside the v1 group so the
	// path is exactly /v1/upload-tus (matching the frontend tusClient base URL).
	tusH, err := v1.NewTUSHandler(svc, "/DATA/Gallery", uploadStore)
	if err != nil {
		zap.L().Fatal("failed to init TUS handler", zap.Error(err))
	}
	// TUS protocol uses POST/PATCH/HEAD/OPTIONS on the base path and child paths.
	e.Any("/v1/upload-tus", echo.WrapHandler(tusH))
	e.Any("/v1/upload-tus/*", echo.WrapHandler(tusH))

	return e
}
