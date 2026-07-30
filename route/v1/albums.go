package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// AlbumsHandler handles album CRUD and asset membership operations.
type AlbumsHandler struct{ svc service.Services }

// NewAlbumsHandler constructs an AlbumsHandler.
func NewAlbumsHandler(svc service.Services) *AlbumsHandler { return &AlbumsHandler{svc} }

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

// fromSmartViewReq 是"智能相册→手动相册"转换端点的请求体,字段名与设计
// 文档(spec 1.2)逐字对齐。
type fromSmartViewReq struct {
	SmartViewID string `json:"smartview_id"`
}

// FromSmartView 把智能相册原地固化为手动相册:停止自动更新,当前成员固化,
// 主题/条件随原智能相册一并删除。
//
// POST /v1/photos/albums/from-smartview
func (h *AlbumsHandler) FromSmartView(c echo.Context) error {
	var req fromSmartViewReq
	if err := c.Bind(&req); err != nil || req.SmartViewID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "smartview_id is required")
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
