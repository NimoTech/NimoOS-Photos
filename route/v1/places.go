package v1

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// PlacesHandler serves the Photos Places feature.
type PlacesHandler struct{ svc service.Services }

// NewPlacesHandler constructs a PlacesHandler.
func NewPlacesHandler(svc service.Services) *PlacesHandler { return &PlacesHandler{svc} }

func placeKey(c echo.Context) (int32, error) {
	id, err := strconv.Atoi(c.Param("key"))
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest, "invalid place key")
	}
	return int32(id), nil
}

func placeUserID(c echo.Context) string {
	if v := c.Request().Header.Get("X-NimoOS-User-ID"); v != "" {
		return v
	}
	return "0"
}

// List — GET /v1/photos/places
func (h *PlacesHandler) List(c echo.Context) error {
	resp, err := h.svc.Places().ListPlaces()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Surface the user's cover overrides on the list payload so rail/hover
	// thumbnails match the detail hero (which already honors the override).
	if ov := h.svc.Places().CoverOverrides(placeUserID(c)); len(ov) > 0 {
		for i := range resp.Places {
			if id, ok := ov[resp.Places[i].Key]; ok {
				resp.Places[i].CoverAssetID = id
			}
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// Get — GET /v1/photos/places/:key
func (h *PlacesHandler) Get(c echo.Context) error {
	key, err := placeKey(c)
	if err != nil {
		return err
	}
	d, err := h.svc.Places().GetPlace(key)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if cover, _ := h.svc.Places().GetCover(placeUserID(c), key); cover != "" {
		d.CoverAssetID = cover
	}
	if ov := h.svc.Places().SpotNameOverrides(placeUserID(c)); len(ov) > 0 {
		for i := range d.Spots {
			if n, ok := ov[d.Spots[i].Key]; ok {
				d.Spots[i].Name = n
			}
		}
	}
	return c.JSON(http.StatusOK, d)
}

// SetSpotName — PUT /v1/photos/places/:key/spot-name  { spotKey, name }
func (h *PlacesHandler) SetSpotName(c echo.Context) error {
	var req struct {
		SpotKey string `json:"spotKey"`
		Name    string `json:"name"`
	}
	if e := c.Bind(&req); e != nil || req.SpotKey == "" || req.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "spotKey and name are required")
	}
	if e := h.svc.Places().SetSpotName(placeUserID(c), req.SpotKey, req.Name); e != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, e.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name})
}

// ResetSpotName — DELETE /v1/photos/places/:key/spot-name  { spotKey }
func (h *PlacesHandler) ResetSpotName(c echo.Context) error {
	var req struct {
		SpotKey string `json:"spotKey"`
	}
	if e := c.Bind(&req); e != nil || req.SpotKey == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "spotKey is required")
	}
	if e := h.svc.Places().ResetSpotName(placeUserID(c), req.SpotKey); e != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, e.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"ok": true})
}

// CoverCandidates — GET /v1/photos/places/:key/cover-candidates
func (h *PlacesHandler) CoverCandidates(c echo.Context) error {
	key, err := placeKey(c)
	if err != nil {
		return err
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	res, err := h.svc.Places().CoverCandidates(key, c.QueryParam("tab"), c.QueryParam("q"), page, 40)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, res)
}

// SetCover — PUT /v1/photos/places/:key/cover
func (h *PlacesHandler) SetCover(c echo.Context) error {
	key, err := placeKey(c)
	if err != nil {
		return err
	}
	var req struct {
		AssetID string `json:"assetId"`
	}
	if e := c.Bind(&req); e != nil || req.AssetID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "assetId is required")
	}
	if e := h.svc.Places().SetCover(placeUserID(c), key, req.AssetID); errors.Is(e, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	} else if e != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, e.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"ok": true, "coverAssetId": req.AssetID})
}

// ResetCover — DELETE /v1/photos/places/:key/cover
func (h *PlacesHandler) ResetCover(c echo.Context) error {
	key, err := placeKey(c)
	if err != nil {
		return err
	}
	if e := h.svc.Places().ResetCover(placeUserID(c), key); e != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, e.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"ok": true})
}

// CreateAlbum — POST /v1/photos/places/:key/album
func (h *PlacesHandler) CreateAlbum(c echo.Context) error {
	key, err := placeKey(c)
	if err != nil {
		return err
	}
	var req struct {
		Name string `json:"name"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if e := c.Bind(&req); e != nil || req.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	id, count, e := h.svc.Places().CreateAlbumFromPlace(key, req.Name, req.From, req.To)
	if errors.Is(e, service.ErrInvalidInput) {
		return echo.NewHTTPError(http.StatusBadRequest, "no photos in this place/trip")
	}
	if e != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, e.Error())
	}
	return c.JSON(http.StatusCreated, map[string]interface{}{"albumId": id, "name": req.Name, "count": count})
}
