package v1

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// AssetsHandler handles media asset CRUD and file serving endpoints.
type AssetsHandler struct {
	svc      service.Services
	thumbDir string
	sprites  *service.SpriteGenerator
}

// NewAssetsHandler constructs an AssetsHandler.
// thumbDir is the filesystem directory that contains <id>/small.jpg and <id>/large.jpg.
// sprites is shared with the Indexer (index-time inline pregeneration and the
// startup backfill) so all three writers dedupe through one in-flight table
// and never race on the same output file.
func NewAssetsHandler(svc service.Services, thumbDir string) *AssetsHandler {
	return &AssetsHandler{
		svc:      svc,
		thumbDir: thumbDir,
		sprites:  svc.Indexer().Sprites(),
	}
}

// Sprite serves (and lazily generates) the hover-preview sprite for a video.
func (h *AssetsHandler) Sprite(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(JWTUserID(c), c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if !strings.HasPrefix(asset.MimeType, "video/") {
		return echo.NewHTTPError(http.StatusNotFound, "not a video")
	}

	// Resolve effective duration; never feed 0 into the fps expression.
	durationMs := asset.DurationMs
	if durationMs <= 0 {
		if ms, perr := ffmpeg.GetDurationMs(asset.FilePath); perr == nil && ms > 0 {
			durationMs = ms
			_ = h.svc.Search().UpdateDurationMs(asset.ID, ms) // best-effort write-back
		}
	}
	if durationMs <= 0 {
		return echo.NewHTTPError(http.StatusNotFound, "duration unknown")
	}

	if _, serr := os.Stat(asset.FilePath); serr != nil {
		return echo.NewHTTPError(http.StatusNotFound, "source missing")
	}

	spritePath := filepath.Join(h.thumbDir, asset.ID, "sprite.jpg")
	if _, gerr := h.sprites.Ensure(asset.FilePath, spritePath, durationMs); gerr != nil {
		if errors.Is(gerr, exec.ErrNotFound) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "ffmpeg unavailable")
		}
		return echo.NewHTTPError(http.StatusServiceUnavailable, gerr.Error())
	}

	frames, frameH, ferr := service.SpriteInfoFromFile(spritePath)
	if ferr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, ferr.Error())
	}

	hdr := c.Response().Header()
	hdr.Set("X-Sprite-Frames", strconv.Itoa(frames))
	hdr.Set("X-Sprite-Frame-W", strconv.Itoa(service.SpriteFrameW))
	hdr.Set("X-Sprite-Frame-H", strconv.Itoa(frameH)) // 实际帧高（按原始比例，从文件读）
	hdr.Set("X-Sprite-Duration-Ms", strconv.FormatInt(durationMs, 10))
	hdr.Set("Cache-Control", "max-age=604800")
	return c.File(spritePath)
}

// List returns a paginated list of assets.
// Query params: limit (default 50, max 200), offset (default 0),
// place_key (city geonameid, int), spot_key ("cityID:gx:gy"),
// spot_lat/spot_lon (optional float; pins the exact spot cluster by centroid).
func (h *AssetsHandler) List(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	placeKeyStr := c.QueryParam("place_key")
	spotKey := c.QueryParam("spot_key")

	var f service.AssetFilter
	if placeKeyStr != "" {
		pk, err := strconv.Atoi(placeKeyStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid place_key")
		}
		f.PlaceKey = int32(pk)
	}
	if spotKey != "" {
		if _, _, _, err := service.ParseSpotKey(spotKey); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid spot_key")
		}
		// Resolve the spot to its exact cluster membership so the library count
		// equals the count shown on the spot dialog. When the caller supplies the
		// tapped spot's centroid (spot_lat/spot_lon), match by centroid so two
		// clusters sharing a grid-cell key don't collapse to the largest one;
		// otherwise fall back to the key-only resolution. The pair must be
		// supplied together; a lone half is a 400. AssetIDs takes precedence in
		// ListAssets; a non-nil empty slice yields zero photos.
		var ids []string
		var err error
		latStr := c.QueryParam("spot_lat")
		lonStr := c.QueryParam("spot_lon")
		if (latStr == "") != (lonStr == "") {
			return echo.NewHTTPError(http.StatusBadRequest, "spot_lat and spot_lon must be provided together")
		}
		if latStr != "" {
			lat, errLat := strconv.ParseFloat(latStr, 64)
			lon, errLon := strconv.ParseFloat(lonStr, 64)
			if errLat != nil || errLon != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "invalid spot_lat/spot_lon")
			}
			if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				return echo.NewHTTPError(http.StatusBadRequest, "spot_lat/spot_lon out of range")
			}
			ids, err = h.svc.Places().SpotMemberIDsAt(spotKey, lat, lon)
		} else {
			ids, err = h.svc.Places().SpotMemberIDs(spotKey)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid spot_key")
		}
		f.SpotKey = spotKey
		f.AssetIDs = ids
	}

	assets, err := h.svc.Search().ListAssets(JWTUserID(c), limit, offset, f)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

// Get returns a single asset by ID.
func (h *AssetsHandler) Get(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(JWTUserID(c), c.Param("id"))
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
	zap.L().Warn("deprecated endpoint /v1/assets/upload used, prefer /v1/upload-tus/",
		zap.String("ip", c.RealIP()),
	)
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

// Delete moves an asset to the trash (soft delete). The original file is moved
// to <gallery>/.trash/<id>/ and the row is flagged with deleted_at. Permanent
// deletion happens only from the Recently Deleted view (see TrashHandler.Purge).
func (h *AssetsHandler) Delete(c echo.Context) error {
	id := c.Param("id")
	if err := h.svc.Trash().TrashAsset(id); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
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

// OCRLines serves an asset's stored OCR lines with normalized quadrilaterals.
// Query param q filters to lines containing it (case-insensitive substring,
// same rule as smart-search OCR matching) — the search-hit highlight path.
// GET /v1/photos/assets/:id/ocr?q=
func (h *AssetsHandler) OCRLines(c echo.Context) error {
	lines, err := h.svc.Search().OCRLines(c.Param("id"), c.QueryParam("q"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"lines": lines})
}

// Original streams the full-resolution original file.
func (h *AssetsHandler) Original(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(JWTUserID(c), c.Param("id"))
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
	asset, err := h.svc.Search().GetAsset(JWTUserID(c), c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if asset.LivePhotoVideoID == "" {
		return echo.NewHTTPError(http.StatusNotFound, "no live photo video")
	}
	liveAsset, err := h.svc.Search().GetAsset(JWTUserID(c), asset.LivePhotoVideoID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return c.File(liveAsset.FilePath)
}
