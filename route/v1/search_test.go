package v1_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// mockSearchTextML is equivalent to mockTextML in the service package's tests:
// it maps any query term to a fixed-direction vector e0=[1,0,...], so KNN
// ordering is fully determined by the assets' vector x component, keeping the test deterministic.
type mockSearchTextML struct{}

func (m *mockSearchTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

// searchFakeServices only implements the Search() method that SearchHandler
// depends on; other methods are satisfied by the embedded service.Services
// interface's zero value (never called — a panic there means the test itself is wrong).
type searchFakeServices struct {
	service.Services
	search *service.SearchService
}

func (f *searchFakeServices) Search() *service.SearchService { return f.search }

// newSearchHarness opens a temp database and inserts n semantic assets with
// strictly decreasing scores (id0 scores highest and sorts first), returning
// a handler ready to call Smart directly.
func newSearchHarness(t *testing.T, n int) *v1.SearchHandler {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search_route.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%d", i)
		ds := 0.95 - float64(i)*0.005 // score gap small enough to stay strictly decreasing for large n without triggering extreme cliffs
		x := 0.03 + ds*0.10
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}

	svc := &searchFakeServices{search: service.NewSearchService(db, &mockSearchTextML{})}
	return v1.NewSearchHandler(svc)
}

func doSmart(t *testing.T, h *v1.SearchHandler, body string) (*httptest.ResponseRecorder, []map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/search/smart", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	require.NoError(t, h.Smart(c))
	var results []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &results))
	return rec, results
}

// TestSmartHandlerDefaultLimitIs50 verifies the default limit was raised from
// 20 to 50: with 60 candidate assets in the library, a request without limit
// should get exactly 50.
func TestSmartHandlerDefaultLimitIs50(t *testing.T) {
	h := newSearchHarness(t, 60)
	rec, results := doSmart(t, h, `{"query":"fish"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, results, 50)
}

// TestSmartHandlerLimitClampedToDefaultWhenOutOfRange verifies the limit
// clamp range is now (0,500]: <=0 or >500 both fall back to the default 50
// (keeping the existing "out-of-range falls back to default" pattern rather
// than clamping to the boundary).
func TestSmartHandlerLimitClampedToDefaultWhenOutOfRange(t *testing.T) {
	h := newSearchHarness(t, 60)

	_, zero := doSmart(t, h, `{"query":"fish","limit":0}`)
	require.Len(t, zero, 50)

	_, negative := doSmart(t, h, `{"query":"fish","limit":-5}`)
	require.Len(t, negative, 50)

	_, tooBig := doSmart(t, h, `{"query":"fish","limit":600}`)
	require.Len(t, tooBig, 50)
}

// TestSmartHandlerLimitWithinRangeIsHonored verifies that a limit within
// (0,500] is honored as-is, including values near the new upper bound of 500
// (using a smaller library size to verify 500 itself isn't wrongly treated as out of range).
func TestSmartHandlerLimitWithinRangeIsHonored(t *testing.T) {
	h := newSearchHarness(t, 10)
	_, results := doSmart(t, h, `{"query":"fish","limit":7}`)
	require.Len(t, results, 7)

	h2 := newSearchHarness(t, 3)
	_, results2 := doSmart(t, h2, `{"query":"fish","limit":500}`)
	require.Len(t, results2, 3, "limit=500 is within the clamp range and should be honored as-is; when the library has fewer, return the actual count")
}

// TestSmartHandlerNegativeOffsetClampsToZero verifies that a negative offset
// in the request body is clamped to zero by the route layer: results for
// offset=-1 should be identical (including order) to not sending offset at all (equivalent to offset=0).
func TestSmartHandlerNegativeOffsetClampsToZero(t *testing.T) {
	h := newSearchHarness(t, 20)

	_, zero := doSmart(t, h, `{"query":"fish","limit":10,"offset":0}`)
	_, negative := doSmart(t, h, `{"query":"fish","limit":10,"offset":-7}`)
	require.Len(t, zero, 10)
	require.Len(t, negative, 10)
	for i := range zero {
		require.Equal(t, zero[i]["id"], negative[i]["id"])
	}
}

// TestSmartHandlerOffsetPagesDeeper verifies that offset>0 returns assets
// further down the ranking (pagination actually advances rather than
// repeating the first page), and that deep-page results all have belowCut=true.
func TestSmartHandlerOffsetPagesDeeper(t *testing.T) {
	h := newSearchHarness(t, 20)

	_, first := doSmart(t, h, `{"query":"fish","limit":5,"offset":0}`)
	_, second := doSmart(t, h, `{"query":"fish","limit":5,"offset":5}`)
	require.Len(t, first, 5)
	require.Len(t, second, 5)

	firstIDs := map[string]bool{}
	for _, a := range first {
		firstIDs[a["id"].(string)] = true
	}
	for _, a := range second {
		require.False(t, firstIDs[a["id"].(string)], "the second page should not overlap with the first page")
		belowCut, _ := a["belowCut"].(bool)
		require.True(t, belowCut, "offset>0 results should all have belowCut=true")
	}
}
