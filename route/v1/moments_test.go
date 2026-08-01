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

// momentsFakeServices only implements the Moments()/Albums()/Search() that
// MomentsHandler depends on; the other methods are satisfied by the zero
// value of the embedded service.Services interface (never called — a
// panic there means the test itself is wrong) — following route/v1's
// existing stub convention (tasks_test.go/views_test.go).
type momentsFakeServices struct {
	service.Services
	moments *service.MomentsService
	albums  *service.AlbumService
	search  *service.SearchService
}

func (f *momentsFakeServices) Moments() *service.MomentsService { return f.moments }
func (f *momentsFakeServices) Albums() *service.AlbumService    { return f.albums }
func (f *momentsFakeServices) Search() *service.SearchService   { return f.search }

// newMomentsHarness opens a temp sqlite DB and wires up the minimal set of
// services MomentsHandler needs. searcher/loadVec/namer are passed nil —
// the list/assets/album/recipes test cases never trigger the engine, and
// the recompute test case keeps the recipe table empty (never entering the
// recomputeRecipe branch), so nil is safe.
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

// zeroMLStub satisfies the text-embedding interface SearchService depends
// on, returning a zero vector — this file's tests don't involve semantic
// search, only Search().ListAssets/GetAsset's by-id query path.
type zeroMLStub struct{}

func (zeroMLStub) CLIPTextEmbed(string) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}

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
			TimeFrom:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			TimeTo:     time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
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
	require.Contains(t, m, "cover_ratio", "cover_ratio should always be present")
	require.Equal(t, float64(0), m["cover_ratio"], "seedOneMoment doesn't create asset_exif, should fall back to 0")
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

// TestMomentsHandler_UpdateRecipesUnknownKindRejected: a kind outside the
// {"trip","theme"} whitelist (e.g. an ops/script typo like "thmee") should
// be a 400, not silently persisted — the engine (recomputeRecipe) only
// Warns and skips unknown kinds, never producing output or erroring, so if
// this layer doesn't catch it, the typo quietly ends up in the DB with no
// user-visible indication.
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
	// The two moments must live under different recipe_keys to coexist —
	// seedOneMoment internally calls SyncRecipeMoments("trip", ...), and a
	// second call with the same recipe would treat the previous one as a
	// "vanished old moment" and delete it (see momentstore.go semantics),
	// so we can't seed twice in a row with the same recipeKey here.
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
	require.Equal(t, "m2", moments[0].ID, "after manual reorder, m2 (ids[0]) should come before m1")
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
	require.True(t, pinned.Manual, "a member persisted via pin should be marked manual=1")
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

	// After hiding: List no longer includes it.
	lc, lrec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(lc))
	var listBody struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(lrec.Body.Bytes(), &listBody))
	require.Len(t, listBody.Moments, 0)

	// After hiding: both the assets and album endpoints return 404.
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

// TestMomentsHandler_DeleteTwiceStillReturns404: covers the edge case
// flagged in the ninth-round final review — the first DELETE succeeds in
// hiding (200), and a second DELETE on the same id should still be 404
// (findMoment is based on ListMoments, which already filters hidden=0, so
// it's treated as "not found" from the second call on), rather than
// mistakenly reporting "already hidden" or another 200.
func TestMomentsHandler_DeleteTwiceStillReturns404(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})

	c1, _ := newEchoCtx(http.MethodDelete, "/v1/photos/moments/m1", nil)
	c1.SetParamNames("id")
	c1.SetParamValues("m1")
	require.NoError(t, h.Delete(c1))

	c2, _ := newEchoCtx(http.MethodDelete, "/v1/photos/moments/m1", nil)
	c2.SetParamNames("id")
	c2.SetParamValues("m1")
	err := h.Delete(c2)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code, "a second DELETE on the same id should still be 404")
}

// ── momentDTO.featured_asset_ids ─────────────────────────────────────────

func TestMomentsHandler_ListIncludesFeaturedAssetIDsExcludingCover(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	// a1 is the cover and featured; a2/a3/a4 are also featured (not the
	// cover); a5 is not featured — TopFeaturedByMoment(3) should exclude
	// cover a1 and take the top 3 by score DESC: a2, a3, a4 (needed for
	// the mosaic three-tile template, proving the cap was raised from 2
	// to 3).
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	draft := service.MomentDraft{
		Moment: service.Moment{
			ID: "m1", RecipeKey: "trip", Title: "Kyoto Trip", CoverAssetID: "a1",
			TimeFrom:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			TimeTo:     time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
			AssetCount: 5,
		},
		Assets: []service.MomentAsset{
			{AssetID: "a1", Featured: true, Score: 5},
			{AssetID: "a2", Featured: true, Score: 4},
			{AssetID: "a3", Featured: true, Score: 3},
			{AssetID: "a4", Featured: true, Score: 2},
			{AssetID: "a5", Featured: false, Score: 1},
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
	require.Equal(t, []string{"a2", "a3", "a4"}, body.Moments[0].FeaturedAssetIDs)
}

func TestMomentsHandler_ListFeaturedAssetIDsEmptyWhenNoneFeatured(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1"}) // sole member is the cover, excluded by Top

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	ids, ok := body.Moments[0]["featured_asset_ids"].([]any)
	require.True(t, ok, "featured_asset_ids should always be an array (even if empty), not null or missing")
	require.Len(t, ids, 0)
}

// ── GET /moments/:id/assets?with_members=1 ──────────────────────────────

func TestMomentsHandler_AssetsWithMembersReturnsShapeWithManualFeatured(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"}) // a1 featured/manual=0, a2 not featured/manual=0
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

	// Without the with_members param, the response must be a bare array,
	// not a {"assets":...} object — during the deploy window, old
	// frontends still parse it as a bare array (see the brief's
	// ambiguity ruling).
	var raw []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "response should parse directly as a bare array")
	require.Len(t, raw, 2)
}

// ── momentDTO.added_this_week ────────────────────────────────────────────

func TestMomentsHandler_ListIncludesAddedThisWeekZeroWhenNoneRecent(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})
	// seedOneMoment goes through SyncRecipeMoments, so new members' added_at
	// is exactly "now" for this call, naturally falling inside the 7-day
	// window — here we backdate them to long ago, to test the 0 scenario.
	_, err := db.Exec(`UPDATE moment_assets SET added_at=1 WHERE moment_id='m1'`)
	require.NoError(t, err)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	// Field is always output (0 included); the frontend only shows it when >0.
	require.Contains(t, body.Moments[0], "added_this_week")
	require.Equal(t, float64(0), body.Moments[0]["added_this_week"])
}

func TestMomentsHandler_ListIncludesAddedThisWeekPositiveWhenRecent(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})
	// seedOneMoment's internal SyncRecipeMoments already stamps added_at as
	// "just now", naturally falling inside the 7-day window, so no extra
	// data setup is needed.

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	require.Equal(t, float64(2), body.Moments[0]["added_this_week"], "both members were added this week")
}

// ── GET /moments with cover_ratio ───────────────────────────────────────

func TestMomentsHandler_ListIncludesCoverRatioNormal(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"}) // cover=a1
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES (?, ?, ?)`, "a1", 1600, 2000)
	require.NoError(t, err)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	require.InDelta(t, 0.8, body.Moments[0]["cover_ratio"], 1e-9)
}

func TestMomentsHandler_ListCoverRatioZeroWhenExifMissing(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"}) // no asset_exif row

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	require.Contains(t, body.Moments[0], "cover_ratio", "cover_ratio should always be present, no omitempty")
	require.Equal(t, float64(0), body.Moments[0]["cover_ratio"])
}

func TestMomentsHandler_ListCoverRatioZeroWhenWidthOrHeightZero(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"}) // cover=a1
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES (?, ?, ?)`, "a1", 0, 2000)
	require.NoError(t, err)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments", nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Moments []map[string]any `json:"moments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Moments, 1)
	require.Equal(t, float64(0), body.Moments[0]["cover_ratio"])
}

// ── GET /moments/:id/assets?with_members=1 with places ──────────────────

func TestMomentsHandler_AssetsWithMembersIncludesPlaces(t *testing.T) {
	h, db, store := newMomentsHarness(t)
	seedOneMoment(t, db, store, "m1", []string{"a1", "a2"})
	_, err := db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, "a1", "Bozeman")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, "a2", "Bozeman")
	require.NoError(t, err)

	c, rec := newEchoCtx(http.MethodGet, "/v1/photos/moments/m1/assets?with_members=1", nil)
	c.SetParamNames("id")
	c.SetParamValues("m1")
	require.NoError(t, h.Assets(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Places []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"places"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{{Name: "Bozeman", Count: 2}}, body.Places)
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
	require.Empty(t, recipes, "a rejected recipe should not be persisted")
}
