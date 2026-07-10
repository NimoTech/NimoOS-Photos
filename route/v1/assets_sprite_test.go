package v1_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// fakeSpriteSvc embeds service.Services and overrides Search() (the one method
// the Sprite handler calls directly) and Indexer() (source of the shared
// SpriteGenerator: NewAssetsHandler now takes sprites from svc.Indexer().Sprites()
// instead of self-constructing, so the harness must supply a real Indexer whose
// thumbDir matches the handler's thumbDir — same instance, no duplicate
// generator/no dedup-table split). (Same pattern as favorites_test.go; named
// differently to avoid colliding with that file's fakeServices in package v1_test.)
type fakeSpriteSvc struct {
	service.Services
	search  *service.SearchService
	indexer *service.Indexer
}

func (f *fakeSpriteSvc) Search() *service.SearchService { return f.search }
func (f *fakeSpriteSvc) Indexer() *service.Indexer      { return f.indexer }

func newSpriteHarness(t *testing.T) (*v1.AssetsHandler, *sql.DB, func()) {
	t.Helper()
	thumb := t.TempDir()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	svc := &fakeSpriteSvc{
		search:  service.NewSearchService(db, nil),
		indexer: service.NewIndexer(db, nil, thumb, 1),
	}
	h := v1.NewAssetsHandler(svc, thumb)
	return h, db, func() { db.Close() }
}

func TestSpriteNotAVideo(t *testing.T) {
	h, db, cleanup := newSpriteHarness(t)
	defer cleanup()
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,status) VALUES('p1','/x/p1.jpg','image/jpeg','indexed')`)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("p1")
	err := h.Sprite(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestSpriteGeneratesAndServes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	h, db, cleanup := newSpriteHarness(t)
	defer cleanup()
	// 造源视频
	dir := t.TempDir()
	src := filepath.Join(dir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=25", "-y", src).Run())
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,duration_ms,status)
		VALUES('v1',?, 'video/mp4', 6000, 'indexed')`, src)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("v1")
	require.NoError(t, h.Sprite(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "6", rec.Header().Get("X-Sprite-Frames")) // 6s → 1帧/s → 6（>下限5，不钳制）
	require.Equal(t, "6000", rec.Header().Get("X-Sprite-Duration-Ms"))
}
