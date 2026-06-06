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
