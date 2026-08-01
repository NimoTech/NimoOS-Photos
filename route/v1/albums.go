package v1

import (
	"archive/zip"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// AlbumsHandler handles album CRUD and asset membership operations.
type AlbumsHandler struct {
	svc service.Services
	// runtimePath is used by Export's query-token JWT check to fetch the
	// public key (same convention as FavoritesHandler); an empty string
	// means test/standalone mode, skipping real JWT validation.
	runtimePath string
}

// NewAlbumsHandler constructs an AlbumsHandler.
func NewAlbumsHandler(svc service.Services, runtimePath string) *AlbumsHandler {
	return &AlbumsHandler{svc: svc, runtimePath: runtimePath}
}

// List returns all albums.
//
// GET /v1/photos/albums
func (h *AlbumsHandler) List(c echo.Context) error {
	albums, err := h.svc.Albums().List()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, albums)
}

// Create creates a new album.
func (h *AlbumsHandler) Create(c echo.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	album, err := h.svc.Albums().Create(req.Name)
	if errors.Is(err, service.ErrInvalidInput) {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	if errors.Is(err, service.ErrAlbumNameExists) {
		return echo.NewHTTPError(http.StatusConflict, "album name already exists")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusCreated, album)
}

// Get returns a single album together with its asset list.
//
// GET /v1/photos/albums/:id
func (h *AlbumsHandler) Get(c echo.Context) error {
	album, err := h.svc.Albums().Get(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	assets, _ := h.svc.Albums().ListAssets(album.ID)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"album":  album,
		"assets": assets,
	})
}

// Export — GET /v1/photos/albums/:id/export?token=<jwt>
//
// Manual album ZIP download, mirroring favorites.Export: browser
// window.location.href navigation can't send an Authorization header, so
// router.go's mediaGetSkip lets the route suffix bypass the JWT middleware,
// and the handler validates a query token itself instead (missing/invalid
// token -> 401). When runtimePath=="" it's a test/standalone scenario and
// real validation is skipped (same convention as FavoritesHandler). Albums
// aren't user-scoped, so this only checks token validity, not the userID.
//
// zip contents = the full set of visible members returned by ListAssets
// (soft-deleted/offline/Live Photo companion videos already excluded, in
// the album's existing order); filename collisions reuse favorites.go's
// dedupZipEntryName; a single file read failure is skipped with a warn log
// (same fallback strategy as favorites.Export).
func (h *AlbumsHandler) Export(c echo.Context) error {
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
	if _, err := h.svc.Albums().Get(id); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	assets, err := h.svc.Albums().ListAssets(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(assets) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "no assets to export")
	}

	filename := fmt.Sprintf("album-%s.zip", id)
	c.Response().Header().Set("Content-Type", "application/zip")
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Response().WriteHeader(http.StatusOK)

	zw := zip.NewWriter(c.Response().Writer)
	defer zw.Close()

	usedNames := map[string]int{}
	for _, a := range assets {
		name := dedupZipEntryName(a.OriginalName, usedNames)
		if name == "" {
			name = a.ID + filepath.Ext(a.FilePath)
		}

		w, err := zw.Create(name)
		if err != nil {
			zap.L().Warn("album zip create entry failed", zap.String("name", name), zap.Error(err))
			continue
		}
		f, err := os.Open(a.FilePath)
		if err != nil {
			zap.L().Warn("album zip skip missing file", zap.String("path", a.FilePath), zap.Error(err))
			continue
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		if copyErr != nil {
			zap.L().Warn("album zip copy failed", zap.String("path", a.FilePath), zap.Error(copyErr))
			continue
		}
		if flusher, ok := c.Response().Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return nil
}

// Delete removes an album (does not delete the underlying assets).
//
// DELETE /v1/photos/albums/:id
func (h *AlbumsHandler) Delete(c echo.Context) error {
	err := h.svc.Albums().Delete(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// AddAsset adds an asset to an album.
//
// POST /v1/photos/albums/:id/assets
//
//	{ "assetId": "<uuid>" }
func (h *AlbumsHandler) AddAsset(c echo.Context) error {
	var req struct {
		AssetID string `json:"assetId"`
	}
	if err := c.Bind(&req); err != nil || req.AssetID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "assetId is required")
	}
	if err := h.svc.Albums().AddAsset(c.Param("id"), req.AssetID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "added"})
}

// RemoveAsset removes an asset from an album.
//
// DELETE /v1/photos/albums/:id/assets/:asset
func (h *AlbumsHandler) RemoveAsset(c echo.Context) error {
	if err := h.svc.Albums().RemoveAsset(c.Param("id"), c.Param("asset")); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// BatchAdd — POST /v1/photos/albums/:id/assets/batch
//
//	{ "assetIds": ["uuid1", "uuid2"] }
func (h *AlbumsHandler) BatchAdd(c echo.Context) error {
	var req struct {
		AssetIDs []string `json:"assetIds"`
	}
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	err := h.svc.Albums().BatchAddAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "album not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// Update partially updates an album (name and/or coverAssetId).
//
// PATCH /v1/photos/albums/:id
//
//	{ "name"?: string, "coverAssetId"?: string }
func (h *AlbumsHandler) Update(c echo.Context) error {
	var req struct {
		Name         *string `json:"name"`
		CoverAssetID *string `json:"coverAssetId"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if req.Name == nil && req.CoverAssetID == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "at least one of name/coverAssetId is required")
	}

	id := c.Param("id")
	if req.Name != nil {
		if err := h.svc.Albums().UpdateName(id, *req.Name); err != nil {
			return mapAlbumErr(err)
		}
	}
	if req.CoverAssetID != nil {
		if err := h.svc.Albums().UpdateCover(id, *req.CoverAssetID); err != nil {
			return mapAlbumErr(err)
		}
	}

	album, err := h.svc.Albums().Get(id)
	if err != nil {
		return mapAlbumErr(err)
	}
	return c.JSON(http.StatusOK, album)
}

// Reorder writes a new full ordering of the album's assets.
//
// PATCH /v1/photos/albums/:id/assets/order
//
//	{ "assetIds": ["uuid1", "uuid2", ...] }
func (h *AlbumsHandler) Reorder(c echo.Context) error {
	var req struct {
		AssetIDs []string `json:"assetIds"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}

	if err := h.svc.Albums().ReorderAssets(c.Param("id"), req.AssetIDs); err != nil {
		return mapAlbumErr(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// fromSmartViewReq is the request body for the "smart view -> manual album"
// conversion endpoint; field names follow the codebase's camelCase convention.
type fromSmartViewReq struct {
	SmartViewID string `json:"smartViewId"`
}

// FromSmartView solidifies a smart view into a manual album in place: auto
// updates stop, current members are frozen, and the topic/conditions are
// deleted along with the original smart view.
//
// POST /v1/photos/albums/from-smartview
func (h *AlbumsHandler) FromSmartView(c echo.Context) error {
	var req fromSmartViewReq
	if err := c.Bind(&req); err != nil || req.SmartViewID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "smartViewId is required")
	}
	album, err := h.svc.SmartViews().ConvertToAlbum(req.SmartViewID)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "smart view not found")
	}
	if errors.Is(err, service.ErrAlbumNameExists) {
		return echo.NewHTTPError(http.StatusConflict, "album name already exists")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, album)
}

// mapAlbumErr maps service-layer errors to HTTP responses.
func mapAlbumErr(err error) error {
	switch {
	case errors.Is(err, service.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound)
	case errors.Is(err, service.ErrAlbumNameExists):
		return echo.NewHTTPError(http.StatusConflict, "album name already exists")
	case errors.Is(err, service.ErrCoverNotInAlbum):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "cover asset not in album")
	case errors.Is(err, service.ErrInvalidInput):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
}
