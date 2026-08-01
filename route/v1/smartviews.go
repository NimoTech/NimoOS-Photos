package v1

import (
	"crypto/ecdsa"
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// SmartViewsHandler handles smart view CRUD, preview, and export endpoints.
type SmartViewsHandler struct {
	svc service.Services
	// runtimePath is used by ExportZip's query-token JWT check to fetch
	// the public key (same convention as AlbumsHandler/FavoritesHandler);
	// an empty string means test/standalone mode, skipping real JWT
	// validation.
	runtimePath string
}

// NewSmartViewsHandler constructs a SmartViewsHandler.
func NewSmartViewsHandler(svc service.Services, runtimePath string) *SmartViewsHandler {
	return &SmartViewsHandler{svc: svc, runtimePath: runtimePath}
}

// RegisterSmartViewRoutes registers all smart-view routes on the given group.
func RegisterSmartViewRoutes(g *echo.Group, h *SmartViewsHandler) {
	g.GET("/smart-views", h.List)
	g.POST("/smart-views", h.Create)
	g.POST("/smart-views/preview", h.Preview)
	g.GET("/smart-views/:id", h.Get)
	g.PUT("/smart-views/:id", h.Update)
	g.DELETE("/smart-views/:id", h.Delete)
	g.POST("/smart-views/:id/duplicate", h.Duplicate)
	g.GET("/smart-views/:id/assets", h.Assets)
	g.POST("/smart-views/:id/assets", h.PinAssets)
	g.POST("/smart-views/:id/assets/remove", h.RemoveAssets)
	g.POST("/smart-views/:id/assets/restore", h.RestoreAssets)
	g.GET("/smart-views/:id/excluded", h.Excluded)
	g.GET("/smart-views/:id/activity", h.Activity)
	// Existing POST /export (format=zip|album) is left unchanged for backward compatibility.
	g.POST("/smart-views/:id/export", h.Export)
	// New GET+token ZIP direct-download endpoint, fixing a broken UI
	// window.location.href link: browser navigation can't send an
	// Authorization header, and the POST-only route can't handle GET,
	// so the old path gets 401'd by the JWT middleware. Mirrors the
	// shape of albums.go's AlbumsHandler.Export.
	g.GET("/smart-views/:id/export", h.ExportZip)
	g.POST("/smart-views/from-album", h.FromAlbum)
}

// svAssetIDsReq is the shared request body for the pin/remove/restore write endpoints.
type svAssetIDsReq struct {
	AssetIDs []string `json:"assetIds"`
}

// PinAssets pins the given assets into the view, returning the count of assets whose state actually changed.
func (h *SmartViewsHandler) PinAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	added, err := h.svc.SmartViews().PinAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"added": added})
}

// RemoveAssets removes assets in a tiered way: pinned rows get unpinned, auto rows get marked excluded.
func (h *SmartViewsHandler) RemoveAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	unpinned, excluded, err := h.svc.SmartViews().RemoveAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"unpinned": unpinned, "excluded": excluded})
}

// RestoreAssets restores excluded assets so they participate in view matching again.
func (h *SmartViewsHandler) RestoreAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	restored, err := h.svc.SmartViews().RestoreAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"restored": restored})
}

// Excluded returns the view's exclusion list (for the collapsible section on the detail page).
func (h *SmartViewsHandler) Excluded(c echo.Context) error {
	assets, err := h.svc.SmartViews().ExcludedAssets(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

func (h *SmartViewsHandler) List(c echo.Context) error {
	list, err := h.svc.SmartViews().List()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, list)
}

func (h *SmartViewsHandler) Create(c echo.Context) error {
	var in service.SmartViewInput
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	sv, err := h.svc.SmartViews().Create(in)
	if errors.Is(err, service.ErrInvalidInput) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid input")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Get(c echo.Context) error {
	sv, err := h.svc.SmartViews().Get(c.Param("id"))
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Update(c echo.Context) error {
	var p service.SmartViewPatch
	if err := c.Bind(&p); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}
	sv, err := h.svc.SmartViews().Update(c.Param("id"), p)
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Delete(c echo.Context) error {
	if err := h.svc.SmartViews().Delete(c.Param("id")); err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *SmartViewsHandler) Duplicate(c echo.Context) error {
	sv, err := h.svc.SmartViews().Duplicate(c.Param("id"))
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Assets(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	recent := c.QueryParam("recent") == "true"
	assets, err := h.svc.SmartViews().MatchedAssets(c.Param("id"), limit, offset, recent, JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

func (h *SmartViewsHandler) Activity(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	acts, err := h.svc.SmartViews().Activity(c.Param("id"), limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, acts)
}

func (h *SmartViewsHandler) Preview(c echo.Context) error {
	var req struct {
		CondsRaw      []string `json:"condsRaw"`
		Description   string   `json:"description"`
		Threshold     int      `json:"threshold"`
		IncludeVideos bool     `json:"includeVideos"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}
	count, ids, thresholdActive, err := h.svc.SmartViews().Preview(req.CondsRaw, req.Description, req.Threshold, req.IncludeVideos)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"count": count, "seeds": ids, "thresholdActive": thresholdActive})
}

// fromAlbumReq is the request body for the "manual album -> smart view"
// conversion endpoint; field names follow the codebase's camelCase
// convention (consistent with response fields like albumId).
type fromAlbumReq struct {
	AlbumID       string   `json:"albumId"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Conds         []string `json:"conds"`
	Threshold     int      `json:"threshold"`
	IncludeVideos bool     `json:"includeVideos"`
}

// FromAlbum turns a manual album into a smart view in place: existing
// members are all locked as pinned, the original album is deleted, and
// Nimo keeps pulling in new photos matching the topic.
func (h *SmartViewsHandler) FromAlbum(c echo.Context) error {
	var req fromAlbumReq
	if err := c.Bind(&req); err != nil || req.AlbumID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "albumId is required")
	}
	sv, err := h.svc.SmartViews().ConvertFromAlbum(service.ConvertFromAlbumInput{
		AlbumID:       req.AlbumID,
		Name:          req.Name,
		Description:   req.Description,
		CondsRaw:      req.Conds,
		Threshold:     req.Threshold,
		IncludeVideos: req.IncludeVideos,
	})
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "album not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Export(c echo.Context) error {
	id := c.Param("id")
	format := c.QueryParam("format")
	if format == "" {
		var req struct {
			Format string `json:"format"`
		}
		_ = c.Bind(&req)
		format = req.Format
	}
	switch format {
	case "album":
		albumID, err := h.svc.SmartViews().ExportAsAlbum(id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]string{"albumId": albumID})
	case "zip":
		if err := h.svc.SmartViews().ExportZip(c.Response(), id); err != nil {
			if errors.Is(err, service.ErrInvalidInput) {
				return echo.NewHTTPError(http.StatusBadRequest, "no matches to export")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return nil
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported format")
	}
}

// ExportZip — GET /v1/photos/smart-views/:id/export?token=<jwt>
//
// Same browser direct-download entry point as albums.go's
// AlbumsHandler.Export: fixes an existing broken UI link —
// PhotosSmartViewDetail.vue's runExport('zip') triggers the download with
// window.location.href, and browser navigation can't send an Authorization
// header, while the old POST /export neither registers GET nor appears in
// router.go's mediaGetSkip allowlist, so the request gets 401'd right at
// the JWT middleware. This switches to self-validating a query token
// (when runtimePath=="" it's a test/standalone scenario, skipping real
// validation, same convention as Albums/Favorites); the streaming
// implementation reuses service.SmartViewService.ExportZip (the exact
// same implementation as the old POST /export?format=zip branch, behavior
// unchanged). The existing POST route is left unchanged; the two coexist
// without affecting each other.
func (h *SmartViewsHandler) ExportZip(c echo.Context) error {
	token := c.QueryParam("token")
	if token == "" {
		return echo.NewHTTPError(http.StatusUnauthorized, "token required")
	}
	if h.runtimePath != "" {
		valid, _, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
			return external.GetPublicKey(h.runtimePath)
		})
		if err != nil || !valid {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
	}

	id := c.Param("id")
	if _, err := h.svc.SmartViews().Get(id); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	if err := h.svc.SmartViews().ExportZip(c.Response(), id); err != nil {
		if errors.Is(err, service.ErrInvalidInput) {
			return echo.NewHTTPError(http.StatusBadRequest, "no matches to export")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return nil
}
