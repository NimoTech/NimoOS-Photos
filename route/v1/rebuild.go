package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// RebuildHandler triggers full AI index rebuilds.
type RebuildHandler struct{ svc service.Services }

// NewRebuildHandler constructs a RebuildHandler.
func NewRebuildHandler(svc service.Services) *RebuildHandler { return &RebuildHandler{svc} }

// POST /v1/photos/index/rebuild
func (h *RebuildHandler) Rebuild(c echo.Context) error {
	taskID, err := h.svc.Rebuilder().Start()
	if err != nil {
		if errors.Is(err, service.ErrRebuildRunning) {
			return echo.NewHTTPError(http.StatusConflict, "rebuild already running")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusAccepted, map[string]string{"taskId": taskID})
}
