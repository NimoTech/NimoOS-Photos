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

// fakePreviewSvc mirrors fakeSpriteSvc (assets_sprite_test.go) — the Preview
// handler goes through the same Search()/Indexer() dependencies as Sprite,
// via the same shared *service.SpriteGenerator from svc.Indexer().Sprites().
type fakePreviewSvc struct {
	service.Services
	search  *service.SearchService
	indexer *service.Indexer
}

func (f *fakePreviewSvc) Search() *service.SearchService { return f.search }
func (f *fakePreviewSvc) Indexer() *service.Indexer      { return f.indexer }

func newPreviewHarness(t *testing.T) (*v1.AssetsHandler, *sql.DB, func()) {
	t.Helper()
	thumb := t.TempDir()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	svc := &fakePreviewSvc{
		search:  service.NewSearchService(db, nil),
		indexer: service.NewIndexer(db, nil, thumb, 1),
	}
	h := v1.NewAssetsHandler(svc, thumb)
	return h, db, func() { db.Close() }
}

func TestPreviewNotAVideo(t *testing.T) {
	h, db, cleanup := newPreviewHarness(t)
	defer cleanup()
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,status) VALUES('p1','/x/p1.jpg','image/jpeg','indexed')`)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("p1")
	err := h.Preview(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestPreviewGeneratesAndServes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	h, db, cleanup := newPreviewHarness(t)
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
	require.NoError(t, h.Preview(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "max-age=604800", rec.Header().Get("Cache-Control"))
	require.NotZero(t, rec.Body.Len(), "response body should carry the generated preview.mp4")

	// 再次请求：preview.mp4 已存在 → ensure 核心秒退缓存命中，仍 200。
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	c2 := e.NewContext(req2, rec2)
	c2.SetParamNames("id")
	c2.SetParamValues("v1")
	require.NoError(t, h.Preview(c2))
	require.Equal(t, http.StatusOK, rec2.Code)
}

func TestPreviewSupportsRange(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	h, db, cleanup := newPreviewHarness(t)
	defer cleanup()
	dir := t.TempDir()
	src := filepath.Join(dir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=25", "-y", src).Run())
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,duration_ms,status)
		VALUES('v1',?, 'video/mp4', 6000, 'indexed')`, src)

	e := echo.New()

	// 先请求一次落地 preview.mp4，避免 Range 请求撞上首次生成路径。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("v1")
	require.NoError(t, h.Preview(c))
	require.Equal(t, http.StatusOK, rec.Code)

	// <video> 标签的 Range 请求应得到 206 + 部分内容（c.File 底层 http.ServeContent 自带）。
	rangeReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rangeReq.Header.Set("Range", "bytes=0-99")
	rangeRec := httptest.NewRecorder()
	rc := e.NewContext(rangeReq, rangeRec)
	rc.SetParamNames("id")
	rc.SetParamValues("v1")
	require.NoError(t, h.Preview(rc))
	require.Equal(t, http.StatusPartialContent, rangeRec.Code)
	require.Equal(t, 100, rangeRec.Body.Len())
	require.NotEmpty(t, rangeRec.Header().Get("Content-Range"))
}
