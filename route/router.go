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
// preview/favorites/export/albums export/smart-views export). These are matched by PATH SUFFIX ONLY (Echo's
// c.Path() is the route pattern, e.g. "/v1/photos/assets/:id/preview", and
// does not encode the HTTP method), so this check MUST be GET-only:
// The method pre-check must not be dropped — otherwise write endpoints sharing
// the same suffix get let through too, e.g. POST /v1/photos/smart-views/preview
// would be wrongly matched by the "/preview" suffix and bypass JWT entirely
// (reproduced before: that POST request without an Authorization header returned 200).
// Every route registered in the media-exemption list is GET, hence the explicit
// GET pre-condition guard here.
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
		strings.HasSuffix(path, "/favorites/export") ||
		// Manual-album ZIP download: window.location.href browser navigation
		// can't send an Authorization header either, same query-token fallback
		// as favorites/export (see albums.go AlbumsHandler.Export). The literal
		// suffix matches the route pattern exactly (":id" is literal text, not
		// a real id), so it won't accidentally catch other /export routes.
		strings.HasSuffix(path, "/albums/:id/export") ||
		// Smart-album ZIP download (new GET+token endpoint fixing the UI's
		// broken location.href link, see smartviews.go SmartViewsHandler.ExportZip):
		// same literal suffix match as albums, and the GET-only pre-check still
		// blocks the existing POST /export (format=zip|album) on the same suffix
		// from being wrongly let through.
		strings.HasSuffix(path, "/smart-views/:id/export")
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
			// /healthz is registered below with a comment claiming no auth is
			// required; the Skipper must actually exempt it to match, else it
			// 401s in production despite the comment (reproduced against prod).
			if p == "/healthz" || p == common.V1APIPath+"/version" {
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
	albums := v1.NewAlbumsHandler(svc, runtimePath)
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
	g.GET("/timeline/buckets", tl.Buckets)
	g.GET("/timeline/bucket", tl.Bucket)

	// Album endpoints
	g.GET("/albums", albums.List)
	g.POST("/albums", albums.Create)
	g.GET("/albums/:id", albums.Get)
	g.GET("/albums/:id/summary", albums.Summary)
	g.DELETE("/albums/:id", albums.Delete)
	g.POST("/albums/:id/assets", albums.AddAsset)
	g.DELETE("/albums/:id/assets/:asset", albums.RemoveAsset)
	g.GET("/albums/:id/export", albums.Export)

	// Album batch
	g.POST("/albums/:id/assets/batch", albums.BatchAdd)
	g.PATCH("/albums/:id", albums.Update)
	g.PATCH("/albums/:id/assets/order", albums.Reorder)

	// In-place manual↔smart album conversion (smart→manual direction; manual→smart
	// is in the smart-views route group)
	g.POST("/albums/from-smartview", albums.FromSmartView)

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

	// Trash
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

	smartViews := v1.NewSmartViewsHandler(svc, runtimePath)
	v1.RegisterSmartViewRoutes(g, smartViews)

	// Smart Moments
	moments := v1.NewMomentsHandler(svc, ctx)
	g.GET("/moments", moments.List)
	g.GET("/moments/:id/assets", moments.Assets)
	g.POST("/moments/:id/assets", moments.PinAssets)
	g.DELETE("/moments/:id/assets", moments.ExcludeAssets)
	g.POST("/moments/:id/album", moments.CreateAlbum)
	g.POST("/moments/recompute", moments.Recompute)
	g.GET("/moments/recipes", moments.ListRecipes)
	g.PUT("/moments/recipes", moments.UpdateRecipes)
	g.PUT("/moments/order", moments.ReorderMoments)
	g.DELETE("/moments/:id", moments.Delete)

	// Construct the upload Store (backed by photos.db), shared by the TUS handler and the uploads API.
	uploadStore := uploadstore.NewStore(svc.DB())

	// Register the uploads list/detail/cancel endpoints.
	uploadTasks := v1.NewUploadTasksHandler(uploadStore)
	g.GET("/uploads", uploadTasks.ListUploads)
	g.GET("/uploads/:id", uploadTasks.GetUpload)
	g.POST("/uploads/:id/cancel", uploadTasks.CancelUpload)

	// Start the tiered GC (background goroutine).
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
