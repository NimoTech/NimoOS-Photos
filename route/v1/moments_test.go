package v1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// momentsFakeServices 只实现 MomentsHandler 依赖的 Moments()/Albums()/Search(),
// 其余方法由内嵌的 service.Services 接口零值满足(未调用到,panic 即视为
// 测试写错)——照 route/v1 既有 stub 惯例(tasks_test.go/views_test.go)。
type momentsFakeServices struct {
	service.Services
	moments *service.MomentsService
	albums  *service.AlbumService
	search  *service.SearchService
}

func (f *momentsFakeServices) Moments() *service.MomentsService { return f.moments }
func (f *momentsFakeServices) Albums() *service.AlbumService    { return f.albums }
func (f *momentsFakeServices) Search() *service.SearchService   { return f.search }

// newMomentsHarness 打开临时 sqlite 库,装配 MomentsHandler 所需的最小服务集。
// searcher/loadVec/namer 传 nil——list/assets/album/recipes 用例都不触发引擎,
// recompute 用例保证 recipe 表为空(不进入 recomputeRecipe 分支),nil 安全。
func newMomentsHarness(t *testing.T) (*MomentsHandler, *sql.DB, *service.MomentStore) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "moments_route.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store := service.NewMomentStore(db)
	momentsSvc := service.NewMomentsService(db, store, nil, nil, nil)
	svc := &momentsFakeServices{
		moments: momentsSvc,
		albums:  service.NewAlbumService(db),
		search:  service.NewSearchService(db, zeroMLStub{}),
	}
	h := NewMomentsHandler(svc, context.Background())
	return h, db, store
}

// zeroMLStub 满足 SearchService 依赖的文本嵌入接口,返回零向量——本文件测试
// 不涉及语义检索,只用 Search().ListAssets/GetAsset 的按 id 查询路径。
type zeroMLStub struct{}

func (zeroMLStub) CLIPTextEmbed(string) ([]float32, error) { return make([]float32, common.CLIPDim), nil }

func newEchoCtx(method, path string, body []byte) (echo.Context, *httptest.ResponseRecorder) {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

func seedOneMoment(t *testing.T, db *sql.DB, store *service.MomentStore, momentID string, assetIDs []string) {
	t.Helper()
	for _, id := range assetIDs {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	assets := make([]service.MomentAsset, 0, len(assetIDs))
	for i, id := range assetIDs {
		assets = append(assets, service.MomentAsset{AssetID: id, Featured: i == 0, Score: float64(len(assetIDs) - i)})
	}
	draft := service.MomentDraft{
		Moment: service.Moment{
			ID: momentID, RecipeKey: "trip", Title: "Kyoto Trip", Subtitle: "3 days",
			CoverAssetID: assetIDs[0], Place: "Kyoto",
			TimeFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			TimeTo:   time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
			AssetCount: len(assetIDs),
		},
		Assets: assets,
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []service.MomentDraft{draft}))
}

func TestMomentsHandler_ListReturnsShapedFields(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	m := body.Moments[0]
	require.Equal(t, "m1", m["id"])
	require.Equal(t, "Kyoto Trip", m["title"])
	require.Equal(t, "3 days", m["subtitle"])
	require.Equal(t, "a1", m["cover_asset_id"])
	require.Equal(t, float64(2), m["asset_count"])
	require.Equal(t, "Kyoto", m["place"])
	require.Equal(t, "trip", m["recipe_key"])
	require.Equal(t, false, m["named_by_llm"])
	require.NotEmpty(t, m["time_from"])
	require.NotEmpty(t, m["time_to"])
}

func TestMomentsHandler_ListEmpty(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Moments)
	require.Len(t, body.Moments, 0)
}

func TestMomentsHandler_AssetsReturnsMembersInScoreOrder(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Assets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var assets []service.Asset
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assets))
	require.Len(t, assets, 2)
	require.Equal(t, "a1", assets[0].ID) // a1 has the higher score (seeded first)
}

func TestMomentsHandler_AssetsFeaturedOnlyFilter(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets?featured=1", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Assets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var assets []service.Asset
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assets))
	require.Len(t, assets, 1)
	require.Equal(t, "a1", assets[0].ID) // only a1 was seeded as Featured
}

func TestMomentsHandler_AssetsNotFound(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/nope/assets", nil)
	c.SetParamNames("id")
	c.SetParamValues("nope")
	err := h.Assets(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
	_ = rec
}

func TestMomentsHandler_CreateAlbumSuccess(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodPost, "/v1/photos/moments/m1/album", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.CreateAlbum(c))
	require.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	albumID, _ := body["albumId"].(string)
	require.NotEmpty(t, albumID)
	require.Equal(t, "Kyoto Trip", body["name"])
	require.Equal(t, float64(2), body["count"])

	assets, err := service.NewAlbumService(db).ListAssets(albumID)
	require.NoError(t, err)
	require.Len(t, assets, 2)
}

func TestMomentsHandler_CreateAlbumNotFound(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodPost, "/v1/photos/moments/nope/album", nil)
	c.SetParamNames("id")
	c.SetParamValues("nope")
	err := h.CreateAlbum(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestMomentsHandler_RecomputeReturns202(t *testing.T) {
	h, _, _ := newMomentsHarness(t) // empty recipe table: RecomputeAll is a no-op

	c, rec := newEchoCtx(http.MethodPost, "/v1/photos/moments/recompute", nil)
	require.NoError(t, h.Recompute(c))
	require.Equal(t, http.StatusAccepted, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "moments", body["task_type"])
}

func TestMomentsHandler_ListRecipesIncludesDisabled(t *testing.T) {
	h, _, store := newMomentsHarness(t)
	require.NoError(t, store.UpsertRecipes([]service.MomentRecipe{
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", ParamsJSON: `{"caption_keywords":["dog"]}`, Enabled: false},
	}))

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/recipes", nil)
	require.NoError(t, h.ListRecipes(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Recipes []map[string]any `json:"recipes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Recipes, 1)
	require.Equal(t, "theme:pets", body.Recipes[0]["key"])
	require.Equal(t, false, body.Recipes[0]["enabled"])
	params, ok := body.Recipes[0]["params"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(10), params["min_assets"]) // ParseParams default fill-in
}

func TestMomentsHandler_UpdateRecipesHotUpdate(t *testing.T) {
	h, _, store := newMomentsHarness(t)

	reqBody, _ := json.Marshal(map[string]any{
		"recipes": []map[string]any{
			{
				"key": "theme:pets", "kind": "theme", "title": "Pet Moments",
				"params":  map[string]any{"caption_keywords": []string{"dog", "cat"}},
				"enabled": true,
			},
		},
	})
	c, rec := newEchoCtx(http.MethodPut, "/v1/photos/moments/recipes", reqBody)
	require.NoError(t, h.UpdateRecipes(c))
	require.Equal(t, http.StatusOK, rec.Code)

	recipes, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes, 1)
	require.Equal(t, "theme:pets", recipes[0].Key)
	require.True(t, recipes[0].Enabled)

	params, err := service.ParseParams(recipes[0])
	require.NoError(t, err)
	require.Equal(t, []string{"dog", "cat"}, params.CaptionKeywords)
}

func TestMomentsHandler_UpdateRecipesEmptyBodyRejected(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodPut, "/v1/photos/moments/recipes", []byte(`{"recipes":[]}`))
	err := h.UpdateRecipes(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

// TestMomentsHandler_UpdateRecipesUnknownKindRejected:kind 不在
// {"trip","theme"} 白名单内(如运维/脚本手误的 "thmee")应 400,而不是静默
// 落库——引擎(recomputeRecipe)对未知 kind 只 Warn 跳过、永不产出/永不报错,
// 若在这一层不拦,typo 会悄悄进库且没有任何用户可见的提示。
func TestMomentsHandler_UpdateRecipesUnknownKindRejected(t *testing.T) {
	h, _, store := newMomentsHarness(t)

	reqBody, _ := json.Marshal(map[string]any{
		"recipes": []map[string]any{
			{"key": "theme:typo", "kind": "thmee", "title": "Typo Kind", "enabled": true},
		},
	})
	c, _ := newEchoCtx(http.MethodPut, "/v1/photos/moments/recipes", reqBody)
	err := h.UpdateRecipes(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)

	recipes, lerr := store.ListRecipes(false)
	require.NoError(t, lerr)
	require.Empty(t, recipes, "被拒绝的 recipe 不应落库")
}
