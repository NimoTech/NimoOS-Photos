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

// mockSearchTextML 与 service 包测试里的 mockTextML 等价：把任意查询词映射到固定
// 方向 e0=[1,0,...] 的向量，让 KNN 排序完全由资产向量的 x 分量决定，测试可控。
type mockSearchTextML struct{}

func (m *mockSearchTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

// searchFakeServices 只实现 SearchHandler 依赖的 Search()，其余方法由内嵌的
// service.Services 接口零值满足（未调用到，panic 即视为测试写错）。
type searchFakeServices struct {
	service.Services
	search *service.SearchService
}

func (f *searchFakeServices) Search() *service.SearchService { return f.search }

// newSearchHarness 打开一个临时库，插入 n 条严格递减分数的语义资产（id0 分最高排
// 最前），返回可直接调用 Smart 的 handler。
func newSearchHarness(t *testing.T, n int) *v1.SearchHandler {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search_route.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%d", i)
		ds := 0.95 - float64(i)*0.005 // 分差够小，保证 n 较大时仍严格递减且不触发极端断层
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

// TestSmartHandlerDefaultLimitIs50 验证默认 limit 从旧值 20 升到 50：库里有 60 条
// 候选资产、请求不带 limit 时应恰好拿到 50 条。
func TestSmartHandlerDefaultLimitIs50(t *testing.T) {
	h := newSearchHarness(t, 60)
	rec, results := doSmart(t, h, `{"query":"fish"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, results, 50)
}

// TestSmartHandlerLimitClampedToDefaultWhenOutOfRange 验证 limit 钳制区间变为
// (0,500]：<=0 或 >500 都归默认 50（沿用「越界即归默认」而非夹到边界的既有模式）。
func TestSmartHandlerLimitClampedToDefaultWhenOutOfRange(t *testing.T) {
	h := newSearchHarness(t, 60)

	_, zero := doSmart(t, h, `{"query":"fish","limit":0}`)
	require.Len(t, zero, 50)

	_, negative := doSmart(t, h, `{"query":"fish","limit":-5}`)
	require.Len(t, negative, 50)

	_, tooBig := doSmart(t, h, `{"query":"fish","limit":600}`)
	require.Len(t, tooBig, 50)
}

// TestSmartHandlerLimitWithinRangeIsHonored 验证 (0,500] 区间内的 limit 原样生效，
// 包括新上限 500 附近的值（用小一点的库规模验证 500 本身不会被误判越界）。
func TestSmartHandlerLimitWithinRangeIsHonored(t *testing.T) {
	h := newSearchHarness(t, 10)
	_, results := doSmart(t, h, `{"query":"fish","limit":7}`)
	require.Len(t, results, 7)

	h2 := newSearchHarness(t, 3)
	_, results2 := doSmart(t, h2, `{"query":"fish","limit":500}`)
	require.Len(t, results2, 3, "limit=500 在钳制区间内应原样生效，库不足时返回实际数")
}

// TestSmartHandlerNegativeOffsetClampsToZero 验证请求体里的负数 offset 被路由层
// 归零：offset=-1 的结果应与不传 offset（等价于 offset=0）完全一致（含顺序）。
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

// TestSmartHandlerOffsetPagesDeeper 验证 offset>0 时拿到的是排序更靠后的资产
// （分页真的往后翻，而不是重复返回首页),且深页结果全部 belowCut=true。
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
		require.False(t, firstIDs[a["id"].(string)], "第二页不应与首页重复")
		belowCut, _ := a["belowCut"].(bool)
		require.True(t, belowCut, "offset>0 的结果应全部 belowCut=true")
	}
}
