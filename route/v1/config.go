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
	effective := dirs
	if len(effective) == 0 {
		effective = service.EnumerateScanRoots()
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"watchDirs":          dirs,
		"effectiveWatchDirs": effective,
		"retentionDays":      retention,
		"facesEnabled":       config.Cfg.FacesEnabled,
		"scenesEnabled":      config.Cfg.ScenesEnabled,
		"ocrEnabled":         config.Cfg.OCREnabled,
		"smartViewEnabled":   config.Cfg.SmartViewEnabled,
		"scanInterval":       config.Cfg.ScanInterval,
	})
}

// PUT /v1/photos/config
func (h *ConfigHandler) UpdateConfig(c echo.Context) error {
	var req struct {
		WatchDirs        []string `json:"watchDirs"`
		RetentionDays    int      `json:"retentionDays"`
		FacesEnabled     *bool `json:"facesEnabled"`
		ScenesEnabled    *bool `json:"scenesEnabled"`
		OCREnabled       *bool `json:"ocrEnabled"`
		SmartViewEnabled *bool `json:"smartViewEnabled"`
		ScanInterval     *int  `json:"scanInterval"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	// watchDirs 为空 ⇒ 显式切换到自动模式（范围 = EnumerateScanRoots，动态跟随
	// 挂载），是合法的全量替换取值，不再当作缺失字段拒绝；口径同
	// pkg/config/config.go 的注释。
	if req.RetentionDays != 0 && (req.RetentionDays < 1 || req.RetentionDays > 365) {
		return echo.NewHTTPError(http.StatusBadRequest, "retentionDays must be between 1 and 365")
	}
	scanInterval := config.Cfg.ScanInterval
	if req.ScanInterval != nil {
		switch *req.ScanInterval {
		case 0, 360, 720, 1440, 10080:
			scanInterval = *req.ScanInterval
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "scanInterval must be one of 0,360,720,1440,10080")
		}
	}
	faces := config.Cfg.FacesEnabled
	if req.FacesEnabled != nil {
		faces = *req.FacesEnabled
	}
	scenes := config.Cfg.ScenesEnabled
	if req.ScenesEnabled != nil {
		scenes = *req.ScenesEnabled
	}
	ocr := config.Cfg.OCREnabled
	if req.OCREnabled != nil {
		ocr = *req.OCREnabled
	}
	smartView := config.Cfg.SmartViewEnabled
	if req.SmartViewEnabled != nil {
		smartView = *req.SmartViewEnabled
	}
	if err := config.Save(config.Settings{
		WatchDirs:        req.WatchDirs,
		RetentionDays:    req.RetentionDays,
		FacesEnabled:     faces,
		ScenesEnabled:    scenes,
		OCREnabled:       ocr,
		SmartViewEnabled: smartView,
		ScanInterval:     scanInterval,
	}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	h.svc.RestartWatcher(req.WatchDirs)
	h.svc.RestartScanTicker(scanInterval)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"watchDirs":        req.WatchDirs,
		"retentionDays":    config.Cfg.RetentionDays,
		"facesEnabled":     config.Cfg.FacesEnabled,
		"scenesEnabled":    config.Cfg.ScenesEnabled,
		"ocrEnabled":       config.Cfg.OCREnabled,
		"smartViewEnabled": config.Cfg.SmartViewEnabled,
		"scanInterval":     config.Cfg.ScanInterval,
	})
}
