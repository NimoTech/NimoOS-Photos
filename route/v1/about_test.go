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

// TestAboutExcludesOffline verifies that the librarySince/indexCoverage stats
// exclude offline=1 assets (removable drive unplugged), consistent with the rest of the display.
func TestAboutExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "off.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// The offline asset's indexed_at is earlier: if not filtered out, librarySince would pick it up.
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
	require.Contains(t, body, `"librarySince":"2024-04-12`, "librarySince should not pick up the offline asset's time")
	require.Contains(t, body, `"indexCoverage":1`, "indexCoverage should not count the offline asset")
}

// TestAboutEmptyLibrary: with an empty library, librarySince/indexLastBuilt are null and coverage is 0.
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
