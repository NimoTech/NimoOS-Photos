package v1

import (
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

type ConfigHandler struct{ svc service.Services }

func NewConfigHandler(svc service.Services) *ConfigHandler { return &ConfigHandler{svc} }

// GET /v1/photos/config
func (h *ConfigHandler) GetConfig(c echo.Context) error {
	dirs := config.Cfg.WatchDirs
	if dirs == nil {
		dirs = []string{}
	}
	retention := config.Cfg.RetentionDays
	if retention <= 0 {
		retention = 30
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"watchDirs":     dirs,
		"retentionDays": retention,
		"facesEnabled":  config.Cfg.FacesEnabled,
	})
}

// PUT /v1/photos/config
func (h *ConfigHandler) UpdateConfig(c echo.Context) error {
	var req struct {
		WatchDirs     []string `json:"watchDirs"`
		RetentionDays int      `json:"retentionDays"`
		FacesEnabled  *bool    `json:"facesEnabled"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	if len(req.WatchDirs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "watchDirs must not be empty")
	}
	if req.RetentionDays != 0 && (req.RetentionDays < 1 || req.RetentionDays > 365) {
		return echo.NewHTTPError(http.StatusBadRequest, "retentionDays must be between 1 and 365")
	}
	faces := config.Cfg.FacesEnabled
	if req.FacesEnabled != nil {
		faces = *req.FacesEnabled
	}
	if err := config.Save(config.Settings{
		WatchDirs:        req.WatchDirs,
		RetentionDays:    req.RetentionDays,
		FacesEnabled:     faces,
		ScenesEnabled:    config.Cfg.ScenesEnabled,
		OCREnabled:       config.Cfg.OCREnabled,
		SmartViewEnabled: config.Cfg.SmartViewEnabled,
	}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	h.svc.RestartWatcher(req.WatchDirs)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"watchDirs":     req.WatchDirs,
		"retentionDays": config.Cfg.RetentionDays,
		"facesEnabled":  config.Cfg.FacesEnabled,
	})
}
