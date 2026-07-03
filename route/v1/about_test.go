package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestAboutReturnsVersionAndStats(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, indexed_at)
		VALUES('a1','/x/a.jpg','indexed','2024-04-12 08:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO photos_meta(key,value) VALUES('index_last_rebuilt','2026-06-01T00:00:00Z')`)
	require.NoError(t, err)

	h := NewAboutHandler(service.NewTestServices(db))
	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Get(e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)))

	body := rec.Body.String()
	require.Contains(t, body, common.PhotosVersion)
	require.Contains(t, body, `"indexCoverage":1`)
	require.Contains(t, body, `"librarySince":"2024-04-12`)
	require.Contains(t, body, `"indexLastBuilt":"2026-06-01T00:00:00Z"`)
}

// TestAboutExcludesOffline 验证 librarySince/indexCoverage 两处统计不计入
// offline=1(移动盘已拔出)的资产,口径与其余展示面一致。
func TestAboutExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "off.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// offline 资产的 indexed_at 更早:若未过滤,librarySince 会取到它。
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, indexed_at) VALUES
		('offline','/media/X/a.jpg','indexed','2020-01-01 08:00:00'),
		('online','/x/b.jpg','indexed','2024-04-12 08:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)
	for _, id := range []string{"offline", "online"} {
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
	}

	h := NewAboutHandler(service.NewTestServices(db))
	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Get(e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)))

	body := rec.Body.String()
	require.Contains(t, body, `"librarySince":"2024-04-12`, "librarySince 不应取到 offline 资产的时间")
	require.Contains(t, body, `"indexCoverage":1`, "indexCoverage 不应计入 offline 资产")
}

// TestAboutEmptyLibrary 空库时 librarySince/indexLastBuilt 为 null、coverage 为 0。
func TestAboutEmptyLibrary(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	h := NewAboutHandler(service.NewTestServices(db))
	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Get(e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)))

	body := rec.Body.String()
	require.Contains(t, body, `"librarySince":null`)
	require.Contains(t, body, `"indexLastBuilt":null`)
	require.Contains(t, body, `"indexCoverage":0`)
}
