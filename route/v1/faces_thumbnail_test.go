package v1_test

// Route-level tests for GET /v1/photos/faces/:id/thumbnail — the arbitrary-
// face-id thumbnail endpoint added for the suggestions inbox (Plan C hard-
// depends on this: a suggestion's candidate face may be a free-floating
// face not attached to any person, so the existing /persons/:id/face-
// thumbnail endpoint, keyed by person id, can't serve it). Mirrors the
// crop/cache mechanics already covered by service/persons_test.go's
// TestFaceThumbnail_CropsAndCaches, but exercises the real HTTP route and
// the by-faceId entry point instead of the by-personId one. JWT-exemption
// is covered separately at the real router layer in
// route/router_test.go (TestJWTExemption_FaceThumbnailByID).

import (
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

func newFacesThumbnailTestEcho(t *testing.T) (*echo.Echo, *sql.DB, string) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ft.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	cacheDir := t.TempDir()
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewPersonsHandler(svc, cacheDir, t.TempDir(), context.Background())
	g.GET("/faces/:id/thumbnail", h.FaceThumbnailByID)
	return e, db, cacheDir
}

func writeFtTestJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
}

// TestFaceThumbnailByID_HappyPath proves the endpoint returns real cropped
// image bytes with a JPEG content type for a face that belongs to no person
// at all — the guard the brief calls out explicitly ("must not care whether
// the face belongs to a person; suggestion faces may be free floaters mid-pass").
func TestFaceThumbnailByID_HappyPath(t *testing.T) {
	e, db, _ := newFacesThumbnailTestEcho(t)

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jpg")
	writeFtTestJPEG(t, srcPath, 400, 300)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1', ?, 'chk', 'indexed')`, srcPath)
	require.NoError(t, err)
	// No face_person row at all -- this face is a free floater, not a member
	// of any person.
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1',?,?)`,
		`{"x1":100,"y1":75,"x2":240,"y2":210}`, sqlite.SerializeFloat32([]float32{1, 0, 0}))
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/faces/f1/thumbnail", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get(echo.HeaderContentType), "image/jpeg")
	require.Greater(t, rec.Body.Len(), 0)
	// JPEG magic bytes.
	require.Equal(t, []byte{0xFF, 0xD8}, rec.Body.Bytes()[:2])

	// Second request hits the on-disk cache and still serves the same bytes.
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/photos/faces/f1/thumbnail", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, rec.Body.Bytes(), rec2.Body.Bytes())
}

// TestFaceThumbnailByID_NotFound proves an unknown face id 404s.
func TestFaceThumbnailByID_NotFound(t *testing.T) {
	e, _, _ := newFacesThumbnailTestEcho(t)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/faces/no-such-face/thumbnail", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestFaceThumbnailByID_OfflineAssetReturnsNotFound proves a face whose
// owning asset is offline/soft-deleted 404s rather than erroring — matching
// FaceThumbnail's own behavior for an unusable source, minus the
// fallbackCoverFace-style fallback (there's nothing else to fall back to
// for one specific face id).
func TestFaceThumbnailByID_OfflineAssetReturnsNotFound(t *testing.T) {
	e, db, _ := newFacesThumbnailTestEcho(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, checksum, status, offline) VALUES('a1', '/x/a1', 'chk', 'indexed', 1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','{}',?)`,
		sqlite.SerializeFloat32([]float32{1, 0, 0}))
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/photos/faces/f1/thumbnail", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
