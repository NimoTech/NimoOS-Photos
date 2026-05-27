package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

type ViewsHandler struct {
	svc service.Services
}

func NewViewsHandler(svc service.Services) *ViewsHandler {
	return &ViewsHandler{svc: svc}
}

// Record — POST /v1/photos/views/:asset_id
func (h *ViewsHandler) Record(c echo.Context) error {
	err := h.svc.Views().Record(JWTUserID(c), c.Param("asset_id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}
