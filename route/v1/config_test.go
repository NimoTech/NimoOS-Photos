package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func setupConfigTest(t *testing.T) *ConfigHandler {
	t.Helper()
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, config.Init(cf, "[photos]\n"))
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return NewConfigHandler(service.NewTestServices(db))
}

// GET 返回三个新开关字段。
func TestGetConfigIncludesNewFlags(t *testing.T) {
	h := setupConfigTest(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h.GetConfig(c))
	body := rec.Body.String()
	require.Contains(t, body, `"scenesEnabled":true`)
	require.Contains(t, body, `"ocrEnabled":true`)
	require.Contains(t, body, `"smartViewEnabled":true`)
}

// PUT 可单独更新 scenesEnabled，未传字段保持现值。
func TestUpdateConfigPartialFlags(t *testing.T) {
	h := setupConfigTest(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		strings.NewReader(`{"watchDirs":["/tmp/x"],"scenesEnabled":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.UpdateConfig(e.NewContext(req, rec)))
	require.False(t, config.Cfg.ScenesEnabled)
	require.True(t, config.Cfg.OCREnabled)       // 未传，保持 true
	require.True(t, config.Cfg.FacesEnabled)      // 未传，保持 true
	require.True(t, config.Cfg.SmartViewEnabled) // 未传，保持 true
}
