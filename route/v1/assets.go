package v1

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// AssetsHandler handles media asset CRUD and file serving endpoints.
type AssetsHandler struct {
	svc      service.Services
	thumbDir string
}

// NewAssetsHandler constructs an AssetsHandler.
// thumbDir is the filesystem directory that contains <id>/small.jpg and <id>/large.jpg.
func NewAssetsHandler(svc service.Services) *AssetsHandler {
	// Derive thumbDir from the same convention used in service/service.go:
	// DataPath/thumbs — inferred via the indexer's thumbDir field via StatusCounts is not
	// directly accessible, so we use the same hardcoded DataPath default.
	// A cleaner option is to pass thumbDir explicitly; left as a follow-up.
	return &AssetsHandler{
		svc:      svc,
		thumbDir: "/DATA/.system_data/photos/thumbs",
	}
}

// List returns a paginated list of assets.
// Query params: limit (default 50, max 200), offset (default 0).
func (h *AssetsHandler) List(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	assets, err := h.svc.Search().ListAssets(limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

// Get returns a single asset by ID.
func (h *AssetsHandler) Get(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, asset)
}

// Upload accepts a multipart file upload, writes it to /DATA/Gallery, and
// enqueues the file for indexing.
func (h *AssetsHandler) Upload(c echo.Context) error {
	file, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "file field required")
	}
	src, err := file.Open()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	defer src.Close()

	destPath := filepath.Join("/DATA/Gallery", filepath.Base(file.Filename))
	dst, err := os.Create(destPath)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to save: "+err.Error())
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	h.svc.Indexer().Enqueue(destPath)
	return c.JSON(http.StatusAccepted, map[string]string{
		"status": "queued",
		"path":   destPath,
	})
}

// Delete removes an asset's database record and its original file (plus its
// live-photo video partner, if any).
func (h *AssetsHandler) Delete(c echo.Context) error {
	id := c.Param("id")
	asset, err := h.svc.Search().GetAsset(id)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// Remove original file from disk.
	os.Remove(asset.FilePath) //nolint:errcheck

	// Remove live-photo video partner if present.
	if asset.LivePhotoVideoID != "" {
		if liveAsset, lerr := h.svc.Search().GetAsset(asset.LivePhotoVideoID); lerr == nil {
			os.Remove(liveAsset.FilePath) //nolint:errcheck
		}
		h.svc.DB().Exec(`DELETE FROM assets WHERE id=?`, asset.LivePhotoVideoID) //nolint:errcheck
	}

	if err := h.svc.Search().DeleteAsset(id); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return c.NoContent(http.StatusNoContent)
}

// Thumbnail serves a pre-generated thumbnail.
// Query param: size = "small" (default) | "large"
func (h *AssetsHandler) Thumbnail(c echo.Context) error {
	id := c.Param("id")
	size := c.QueryParam("size")
	if size != "large" {
		size = "small"
	}
	path := filepath.Join(h.thumbDir, id, size+".jpg")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return echo.NewHTTPError(http.StatusNotFound, "thumbnail not ready")
	}
	return c.File(path)
}

// Original streams the full-resolution original file.
func (h *AssetsHandler) Original(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.File(asset.FilePath)
}

// Live streams the video component of a live photo.
func (h *AssetsHandler) Live(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if asset.LivePhotoVideoID == "" {
		return echo.NewHTTPError(http.StatusNotFound, "no live photo video")
	}
	liveAsset, err := h.svc.Search().GetAsset(asset.LivePhotoVideoID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return c.File(liveAsset.FilePath)
}
