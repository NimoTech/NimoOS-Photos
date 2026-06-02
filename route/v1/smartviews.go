package v1

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/NimoTech/NimoOS-Photos/service"
)

// SmartViewsHandler handles smart view CRUD, preview, and export endpoints.
type SmartViewsHandler struct{ svc service.Services }

// NewSmartViewsHandler constructs a SmartViewsHandler.
func NewSmartViewsHandler(svc service.Services) *SmartViewsHandler { return &SmartViewsHandler{svc} }

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
	g.GET("/smart-views/:id/activity", h.Activity)
	g.POST("/smart-views/:id/export", h.Export)
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
	assets, err := h.svc.SmartViews().MatchedAssets(c.Param("id"), limit, offset, recent)
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
		CondsRaw  []string `json:"condsRaw"`
		Threshold int      `json:"threshold"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}
	count, ids, err := h.svc.SmartViews().Preview(req.CondsRaw, req.Threshold)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"count": count, "seeds": ids})
}

func (h *SmartViewsHandler) Export(c echo.Context) error {
	id := c.Param("id")
	format := c.QueryParam("format")
	if format == "" {
		var req struct{ Format string `json:"format"` }
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
		return h.svc.SmartViews().ExportZip(c.Response(), id)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported format")
	}
}
