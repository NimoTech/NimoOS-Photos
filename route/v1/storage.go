package v1

import (
	"net/http"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// StorageHandler exposes storage statistics for the settings page.
type StorageHandler struct{ svc service.Services }

// NewStorageHandler constructs a StorageHandler.
func NewStorageHandler(svc service.Services) *StorageHandler { return &StorageHandler{svc} }

// GET /v1/photos/storage
func (h *StorageHandler) Get(c echo.Context) error {
	st, err := h.svc.Storage().Stats()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, st)
}

// POST /v1/photos/cache/prune
func (h *StorageHandler) Prune(c echo.Context) error {
	res, err := h.svc.Storage().Prune(common.StagingDir, time.Duration(common.StagingMaxAge)*time.Hour)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, res)
}
