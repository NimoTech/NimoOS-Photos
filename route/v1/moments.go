// Smart Moments 的 HTTP 路由层:列表/成员/固化相册/触发重算/recipe 热更新
// 推送入口。数据层(MomentStore)与调度层(MomentsService)见 service 包的
// Task 1-4;本文件只做参数绑定、错误映射、DTO 转换,不含业务逻辑。
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// MomentsHandler handles the Smart Moments API.
type MomentsHandler struct {
	svc service.Services
	ctx context.Context // 应用生命周期 ctx,喂给后台 Recompute goroutine——绝不能
	// 用请求的 c.Request().Context(),那个 ctx 在 handler 返回、响应写完后就会
	// 被 net/http 取消,会连带杀死刚 go 出去的重算(照 PersonsHandler.Recluster
	// 同款教训)。
}

// NewMomentsHandler constructs a MomentsHandler.
func NewMomentsHandler(svc service.Services, ctx context.Context) *MomentsHandler {
	return &MomentsHandler{svc: svc, ctx: ctx}
}

// momentResponse 是 GET /v1/photos/moments 单条时刻的对外字段——蛇形命名照
// 简报权威契约逐字对齐(不同于本文件其余接口沿用的 camelCase 惯例)。
type momentResponse struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle"`
	CoverAssetID string  `json:"cover_asset_id,omitempty"`
	AssetCount   int     `json:"asset_count"`
	TimeFrom     *string `json:"time_from,omitempty"`
	TimeTo       *string `json:"time_to,omitempty"`
	Place        string  `json:"place,omitempty"`
	RecipeKey    string  `json:"recipe_key"`
	NamedByLLM   bool    `json:"named_by_llm"`
}

// toMomentResponse 转换 service.Moment → momentResponse。TimeFrom/TimeTo 为零值
// time.Time 时(主题类时刻没有固定时间窗)对应字段留空(nil,JSON 里省略),
// 非零值格式化为 RFC3339。
func toMomentResponse(m service.Moment) momentResponse {
	r := momentResponse{
		ID:           m.ID,
		Title:        m.Title,
		Subtitle:     m.Subtitle,
		CoverAssetID: m.CoverAssetID,
		AssetCount:   m.AssetCount,
		Place:        m.Place,
		RecipeKey:    m.RecipeKey,
		NamedByLLM:   m.NamedByLLM,
	}
	if !m.TimeFrom.IsZero() {
		s := m.TimeFrom.UTC().Format("2006-01-02T15:04:05Z07:00")
		r.TimeFrom = &s
	}
	if !m.TimeTo.IsZero() {
		s := m.TimeTo.UTC().Format("2006-01-02T15:04:05Z07:00")
		r.TimeTo = &s
	}
	return r
}

// List returns all moments in the shape the brief mandates.
//
// GET /v1/photos/moments
func (h *MomentsHandler) List(c echo.Context) error {
	moments, err := h.svc.Moments().Store().ListMoments()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]momentResponse, 0, len(moments))
	for _, m := range moments {
		out = append(out, toMomentResponse(m))
	}
	return c.JSON(http.StatusOK, map[string]any{"moments": out})
}

// findMoment looks up a single moment by id via ListMoments (MomentStore has
// no single-row getter — the store's contract is list + assets + recipes
// only, per the Task 5 brief). Moment counts are small (a handful of active
// trips/themes), so an O(n) scan here is not worth adding a new store method for.
func (h *MomentsHandler) findMoment(id string) (service.Moment, error) {
	moments, err := h.svc.Moments().Store().ListMoments()
	if err != nil {
		return service.Moment{}, err
	}
	for _, m := range moments {
		if m.ID == id {
			return m, nil
		}
	}
	return service.Moment{}, service.ErrNotFound
}

// Assets returns a moment's member assets, serialized via the same asset
// shape as GET /v1/photos/assets. Query param featured=1 restricts to the
// featured (精选) subset; members are already ordered by score DESC by
// MomentStore.GetMomentAssets, and that order is preserved end-to-end because
// Search().ListAssets short-circuits on a non-nil AssetIDs filter without
// re-sorting.
//
// GET /v1/photos/moments/:id/assets?featured=1
func (h *MomentsHandler) Assets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}

	featuredOnly := c.QueryParam("featured") == "1"
	members, err := h.svc.Moments().Store().GetMomentAssets(id, featuredOnly)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.AssetID)
	}
	assets, err := h.svc.Search().ListAssets(JWTUserID(c), 0, 0, service.AssetFilter{AssetIDs: ids})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

// CreateAlbum 把一个时刻固化为普通相册(全量成员,不限精选),名字用
// moment 的 title——转调既有 AlbumService.Create + BatchAddAssets(照
// smartview.go ExportAsAlbum 同款薄封装,但直接在 handler 里写,不改
// smartview.go)。
//
// POST /v1/photos/moments/:id/album
func (h *MomentsHandler) CreateAlbum(c echo.Context) error {
	id := c.Param("id")
	moment, err := h.findMoment(id)
	if err != nil {
		return mapMomentErr(err)
	}

	members, err := h.svc.Moments().Store().GetMomentAssets(id, false)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.AssetID)
	}

	album, err := h.svc.Albums().Create(moment.Title)
	if err != nil {
		return mapAlbumErr(err)
	}
	if len(ids) > 0 {
		if err := h.svc.Albums().BatchAddAssets(album.ID, ids); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"albumId": album.ID,
		"name":    moment.Title,
		"count":   len(ids),
	})
}

// Recompute 异步触发全量重算(RecomputeAll 自身 CAS 防重入,重入直接返回
// nil),不阻塞请求;进度走既有 TaskRegistry(Type:"moments"),这里只回一个
// 202 + task_type 让前端知道去哪张 task 轮询。
//
// POST /v1/photos/moments/recompute
func (h *MomentsHandler) Recompute(c echo.Context) error {
	go func() {
		if err := h.svc.Moments().RecomputeAll(h.ctx); err != nil {
			zap.L().Warn("moments: 手动触发重算失败", zap.Error(err))
		}
	}()
	return c.JSON(http.StatusAccepted, map[string]string{"task_type": "moments"})
}

// recipeDTO 是 recipe 热更新接口的对外形状:Params 展开为解析后的
// RecipeParams(而非内部存储的原始 JSON 字符串),GET 侧经 ParseParams 补齐
// 默认值,PUT 侧序列化回 ParamsJSON 落库。
type recipeDTO struct {
	Key       string               `json:"key"`
	Kind      string               `json:"kind"`
	Title     string               `json:"title"`
	Params    service.RecipeParams `json:"params"`
	Enabled   bool                 `json:"enabled"`
	UpdatedAt int64                `json:"updated_at,omitempty"`
}

// ListRecipes 列出全部 recipe(含禁用),供 recipe 管理界面展示/编辑。
//
// GET /v1/photos/moments/recipes
func (h *MomentsHandler) ListRecipes(c echo.Context) error {
	recipes, err := h.svc.Moments().Store().ListRecipes(false)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]recipeDTO, 0, len(recipes))
	for _, r := range recipes {
		params, perr := service.ParseParams(r)
		if perr != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, perr.Error())
		}
		out = append(out, recipeDTO{
			Key: r.Key, Kind: r.Kind, Title: r.Title,
			Params: params, Enabled: r.Enabled, UpdatedAt: r.UpdatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"recipes": out})
}

// UpdateRecipes 是 recipe 的热更新推送入口:整批 upsert(按 key),下一轮
// RecomputeAll 即生效,无需改代码/重启服务。
//
// PUT /v1/photos/moments/recipes
//
//	{ "recipes": [{ "key", "kind", "title", "params", "enabled" }, ...] }
func (h *MomentsHandler) UpdateRecipes(c echo.Context) error {
	var req struct {
		Recipes []recipeDTO `json:"recipes"`
	}
	if err := c.Bind(&req); err != nil || len(req.Recipes) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "recipes is required")
	}

	recipes := make([]service.MomentRecipe, 0, len(req.Recipes))
	for _, dto := range req.Recipes {
		if dto.Key == "" || dto.Kind == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "recipe key/kind is required")
		}
		paramsJSON, err := json.Marshal(dto.Params)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		recipes = append(recipes, service.MomentRecipe{
			Key:        dto.Key,
			Kind:       dto.Kind,
			Title:      dto.Title,
			ParamsJSON: string(paramsJSON),
			Enabled:    dto.Enabled,
		})
	}

	if err := h.svc.Moments().Store().UpsertRecipes(recipes); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"updated": len(recipes)})
}

// mapMomentErr maps moment-lookup errors to HTTP responses.
func mapMomentErr(err error) error {
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}
