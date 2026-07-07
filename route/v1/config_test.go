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

// PUT watchDirs 为空清单必须放行（= 显式切换到自动模式，范围由
// EnumerateScanRoots 动态决定），不得 400。回归覆盖：终审发现 main 遗留的
// “watchDirs must not be empty” 校验会拦掉规格承诺的自动模式切换，以及新装机
// 默认 watchDirs 为空时前端 GET 回填 [] 再 PUT 回写会被 400 挡住的场景。
func TestUpdateConfigEmptyWatchDirsSwitchesToAutoMode(t *testing.T) {
	h := setupConfigTest(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		strings.NewReader(`{"watchDirs":[],"scenesEnabled":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.UpdateConfig(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, config.Cfg.WatchDirs, "空清单应落地为自动模式，不得保留旧值或报错")
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
