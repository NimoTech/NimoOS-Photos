package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func getConfigJSON(t *testing.T, h *ConfigHandler) map[string]interface{} {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/config", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.GetConfig(c))
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func TestGetConfig_EffectiveWatchDirs_ManualMode(t *testing.T) {
	h := setupConfigTest(t)
	orig := config.Cfg.WatchDirs
	t.Cleanup(func() { config.Cfg.WatchDirs = orig })
	config.Cfg.WatchDirs = []string{"/DATA/Gallery"}

	body := getConfigJSON(t, h)
	require.Equal(t, []interface{}{"/DATA/Gallery"}, body["effectiveWatchDirs"])
}

func TestGetConfig_EffectiveWatchDirs_AutoModeNonEmpty(t *testing.T) {
	h := setupConfigTest(t)
	orig := config.Cfg.WatchDirs
	t.Cleanup(func() { config.Cfg.WatchDirs = orig })
	config.Cfg.WatchDirs = nil

	body := getConfigJSON(t, h)
	eff, ok := body["effectiveWatchDirs"].([]interface{})
	require.True(t, ok, "effectiveWatchDirs must be an array")
	require.NotEmpty(t, eff) // EnumerateScanRoots 兜底返回 ["/DATA"]
}
