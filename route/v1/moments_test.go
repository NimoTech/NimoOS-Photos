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
// ── PUT /v1/photos/moments/order ────────────────────────────────────────

func TestMomentsHandler_ReorderEmptyIdsRejected(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodPut, "/v1/photos/moments/order", []byte(`{"ids":[]}`))
	err := h.ReorderMoments(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestMomentsHandler_ReorderSuccess(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	// 两个 moment 落在不同 recipe_key 下,才能共存——seedOneMoment 内部走
	// SyncRecipeMoments("trip", ...),同 recipe 二次调用会把前一个当"消失的
	// 旧时刻"删掉(见 momentstore.go 语义),故这里不能用同一 recipeKey 连续
	// seed 两次。
	seedOneMoment(t, db, store, "m1", []string{"a1"})
	for _, id := range []string{"a2"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	draft2 := service.MomentDraft{
		Moment: service.Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Pet Moments", AssetCount: 1},
		Assets: []service.MomentAsset{{AssetID: "a2"}},
	}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []service.MomentDraft{draft2}))

	reqBody, _ := json.Marshal(map[string]any{"ids": []string{"m2", "m1"}})
	c, rec := newEchoCtx(http.MethodPut, "/v1/photos/moments/order", reqBody)
	require.NoError(t, h.ReorderMoments(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body["ok"])

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2)
	require.Equal(t, "m2", moments[0].ID, "手排后 m2(ids[0])应排在 m1 前面")
	require.Equal(t, "m1", moments[1].ID)
}

// ── POST/DELETE /v1/photos/moments/:id/assets(pin/exclude)──────────────

func TestMomentsHandler_PinAssetsAddsMemberAndReturnsCount(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, "a3", "/p/a3.jpg")
	require.NoError(t, err)

	reqBody, _ := json.Marshal(map[string]any{"ids": []string{"a3"}})
	c, rec := newEchoCtx(http.MethodPost, "/v1/photos/moments/m1/assets", reqBody)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.PinAssets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body["ok"])
	require.Equal(t, float64(3), body["asset_count"])

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 3)
	var pinned service.MomentAsset
	for _, m := range members {
		if m.AssetID == "a3" {
			pinned = m
		}
	}
	require.True(t, pinned.Manual, "pin 落库的成员应标 manual=1")
}

func TestMomentsHandler_PinAssetsEmptyIdsRejected(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, _ := newEchoCtx(http.MethodPost, "/v1/photos/moments/m1/assets", []byte(`{"ids":[]}`))
	c.SetParamNames("id")
	c.SetParamValues("m1")
	err := h.PinAssets(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestMomentsHandler_PinAssetsNotFound(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodPost, "/v1/photos/moments/nope/assets", []byte(`{"ids":["a1"]}`))
	c.SetParamNames("id")
	c.SetParamValues("nope")
	err := h.PinAssets(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestMomentsHandler_ExcludeAssetsRemovesMemberAndReturnsCount(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	reqBody, _ := json.Marshal(map[string]any{"ids": []string{"a1"}})
	c, rec := newEchoCtx(http.MethodDelete, "/v1/photos/moments/m1/assets", reqBody)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.ExcludeAssets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body["ok"])
	require.Equal(t, float64(1), body["asset_count"])

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "a2", members[0].AssetID)
}

func TestMomentsHandler_ExcludeAssetsEmptyIdsRejected(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, _ := newEchoCtx(http.MethodDelete, "/v1/photos/moments/m1/assets", []byte(`{"ids":[]}`))
	c.SetParamNames("id")
	c.SetParamValues("m1")
	err := h.ExcludeAssets(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestMomentsHandler_ExcludeAssetsNotFound(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodDelete, "/v1/photos/moments/nope/assets", []byte(`{"ids":["a1"]}`))
	c.SetParamNames("id")
	c.SetParamValues("nope")
	err := h.ExcludeAssets(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

// ── DELETE /v1/photos/moments/:id(hide)──────────────────────────────────

func TestMomentsHandler_DeleteHidesMomentAndFollowUpsReturn404(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodDelete, "/v1/photos/moments/m1", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Delete(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body["ok"])

	// 隐藏后:List 不再包含它。
	lc, lrec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(lc))
	var listBody struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(lrec.Body.Bytes(), &listBody))
	require.Len(t, listBody.Moments, 0)

	// 隐藏后:assets/album 端点均 404。
	ac, _ := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets", nil)
	ac.SetParamNames("id")
	ac.SetParamValues("m1")
	err := h.Assets(ac)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)

	cc, _ := newEchoCtx(http.MethodPost, "/v1/photos/moments/m1/album", nil)
	cc.SetParamNames("id")
	cc.SetParamValues("m1")
	err = h.CreateAlbum(cc)
	he, ok = err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestMomentsHandler_DeleteNotFound(t *testing.T) {
	h, _, _ := newMomentsHarness(t)

	c, _ := newEchoCtx(http.MethodDelete, "/v1/photos/moments/nope", nil)
	c.SetParamNames("id")
	c.SetParamValues("nope")
	err := h.Delete(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

// ── momentDTO.featured_asset_ids ─────────────────────────────────────────

func TestMomentsHandler_ListIncludesFeaturedAssetIDsExcludingCover(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	// a1 是封面且 featured,a2/a3 也 featured(非封面),a4 不 featured——
	// TopFeaturedByMoment(2) 应排除封面 a1,按 score 降序截取前 2:a2、a3。
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	draft := service.MomentDraft{
		Moment: service.Moment{
			ID: "m1", RecipeKey: "trip", Title: "Kyoto Trip", CoverAssetID: "a1",
			TimeFrom:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			TimeTo:     time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
			AssetCount: 4,
		},
		Assets: []service.MomentAsset{
			{AssetID: "a1", Featured: true, Score: 4},
			{AssetID: "a2", Featured: true, Score: 3},
			{AssetID: "a3", Featured: true, Score: 2},
			{AssetID: "a4", Featured: false, Score: 1},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []service.MomentDraft{draft}))

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []struct {
			ID               string   `json:"id"`
			FeaturedAssetIDs []string `json:"featured_asset_ids"`
		} `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	require.Equal(t, []string{"a2", "a3"}, body.Moments[0].FeaturedAssetIDs)
}

func TestMomentsHandler_ListFeaturedAssetIDsEmptyWhenNoneFeatured(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1"}) // 唯一成员即封面,被 Top 排除

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	ids, ok := body.Moments[0]["featured_asset_ids"].([]any)
	require.True(t, ok, "featured_asset_ids 字段应始终是数组(即使为空),不是 null 或缺失")
	require.Len(t, ids, 0)
}

// ── GET /moments/:id/assets?with_members=1 ──────────────────────────────

func TestMomentsHandler_AssetsWithMembersReturnsShapeWithManualFeatured(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"}) // a1 featured/manual=0,a2 非 featured/manual=0
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, "a3", "/p/a3.jpg")
	require.NoError(t, err)
	_, err = store.PinMomentAssets("m1", []string{"a3"}) // manual=1/featured=0
	require.NoError(t, err)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets?with_members=1", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Assets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Assets  []service.Asset `json:"assets"`
		Members []struct {
			AssetID  string `json:"asset_id"`
			Manual   bool   `json:"manual"`
			Featured bool   `json:"featured"`
		} `json:"members"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Assets, 3)
	require.Len(t, body.Members, 3)

	type flags struct{ Manual, Featured bool }
	byID := map[string]flags{}
	for _, m := range body.Members {
		byID[m.AssetID] = flags{m.Manual, m.Featured}
	}
	require.Equal(t, flags{Manual: false, Featured: true}, byID["a1"])
	require.Equal(t, flags{Manual: false, Featured: false}, byID["a2"])
	require.Equal(t, flags{Manual: true, Featured: false}, byID["a3"])
}

func TestMomentsHandler_AssetsWithoutParamStaysBareArray(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Assets(c))

	// 不带 with_members 参数时响应必须是裸数组,不能是 {"assets":...} 对象——
	// 部署窗口内旧前端仍按裸数组解析(见简报歧义裁决)。
	var raw []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "响应应能直接解析为裸数组")
	require.Len(t, raw, 2)
}

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
