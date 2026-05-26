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
