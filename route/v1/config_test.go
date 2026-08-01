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

// GET returns the three new toggle fields.
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

// PUT with an empty watchDirs list must be let through (= an explicit switch
// to auto mode, scope decided dynamically by EnumerateScanRoots), not 400.
// Regression coverage: final review found a "watchDirs must not be empty"
// check left over on main that would block the auto-mode switch promised by
// the spec, as well as the fresh-install scenario where the frontend GETs an
// empty default watchDirs, fills it back as [], and PUTs it — which would be
// blocked with 400.
func TestUpdateConfigEmptyWatchDirsSwitchesToAutoMode(t *testing.T) {
	h := setupConfigTest(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		strings.NewReader(`{"watchDirs":[],"scenesEnabled":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.UpdateConfig(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, config.Cfg.WatchDirs, "an empty list should land as auto mode, not keep the old value or error")
}

// PUT can update scenesEnabled alone; omitted fields keep their current value.
func TestUpdateConfigPartialFlags(t *testing.T) {
	h := setupConfigTest(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/",
		strings.NewReader(`{"watchDirs":["/tmp/x"],"scenesEnabled":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.UpdateConfig(e.NewContext(req, rec)))
	require.False(t, config.Cfg.ScenesEnabled)
	require.True(t, config.Cfg.OCREnabled)       // not sent, stays true
	require.True(t, config.Cfg.FacesEnabled)     // not sent, stays true
	require.True(t, config.Cfg.SmartViewEnabled) // not sent, stays true
}
