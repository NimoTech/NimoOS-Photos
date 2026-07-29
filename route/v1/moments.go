// Smart Moments 的 HTTP 路由层:列表/成员/固化相册/触发重算/recipe 热更新
// 推送入口。数据层(MomentStore)与调度层(MomentsService)见 service 包的
// Task 1-4;本文件只做参数绑定、错误映射、DTO 转换,不含业务逻辑。
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	// SortOrder 是拖拽手排序的序号(nil=未手排),供前端调试用,不强依赖。
	SortOrder *int `json:"sort_order,omitempty"`
	// FeaturedAssetIDs 是该时刻的精选成员 id(不含封面,已按 score 降序截取
	// 前 maxFeaturedAssetIDsPerMoment 个),供列表页直接渲染小图带而不必逐条
	// 请求 /assets?featured=1。恒为数组(可能为空 []),不用 omitempty——前端
	// 不必对该字段做 null 判断。
	FeaturedAssetIDs []string `json:"featured_asset_ids"`
	// AddedThisWeek 是该时刻本周(7 天窗)新增的成员数(added_at 非 NULL 且
	// 落在窗口内才计入,见 MomentStore.AddedThisWeekByMoment)。恒输出(0 也
	// 带),前端判 >0 才显示绿色 "+N this week" 标记。
	AddedThisWeek int `json:"added_this_week"`
}

// maxFeaturedAssetIDsPerMoment 是 List() 合成 featured_asset_ids 时每个时刻
// 截取的精选数量上限,喂给 MomentStore.TopFeaturedByMoment 的 perMoment 参数。
const maxFeaturedAssetIDsPerMoment = 2

// toMomentResponse 转换 service.Moment → momentResponse。TimeFrom/TimeTo 为零值
// time.Time 时(主题类时刻没有固定时间窗)对应字段留空(nil,JSON 里省略),
// 非零值格式化为 RFC3339。featuredAssetIDs/addedThisWeek 均由调用方一次性
// 算好传入(List() 对全部时刻各只查一次 TopFeaturedByMoment/
// AddedThisWeekByMoment,避免逐条时刻查询的 N+1)。
func toMomentResponse(m service.Moment, featuredAssetIDs []string, addedThisWeek int) momentResponse {
	r := momentResponse{
		ID:               m.ID,
		Title:            m.Title,
		Subtitle:         m.Subtitle,
		CoverAssetID:     m.CoverAssetID,
		AssetCount:       m.AssetCount,
		Place:            m.Place,
		RecipeKey:        m.RecipeKey,
		NamedByLLM:       m.NamedByLLM,
		SortOrder:        m.SortOrder,
		FeaturedAssetIDs: featuredAssetIDs,
		AddedThisWeek:    addedThisWeek,
	}
	if r.FeaturedAssetIDs == nil {
		r.FeaturedAssetIDs = []string{}
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
	// 一次查询取全库 featured_asset_ids(排除各自封面),按 moment id 分发——
	// 不对每条时刻单独查询,避免 N+1(TopFeaturedByMoment 本身就是一条 SQL)。
	featured, err := h.svc.Moments().Store().TopFeaturedByMoment(maxFeaturedAssetIDsPerMoment)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// 同法:一次查询取全库 added_this_week,按 moment id 分发,无 N+1。
	addedThisWeek, err := h.svc.Moments().Store().AddedThisWeekByMoment(time.Now().UnixMilli())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]momentResponse, 0, len(moments))
	for _, m := range moments {
		out = append(out, toMomentResponse(m, featured[m.ID], addedThisWeek[m.ID]))
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

// momentAssetMemberDTO 是 with_members=1 附带的成员元数据,供编辑 UI 区分
// "引擎本轮产出"与"用户手动 pin"的成员、以及是否属于精选。
type momentAssetMemberDTO struct {
	AssetID  string `json:"asset_id"`
	Manual   bool   `json:"manual"`
	Featured bool   `json:"featured"`
}

// momentPlaceDTO 是 with_members=1 附带的 About 多地点聚合数据(见设计 spec
// 第三节),转自 service.MomentPlace。
type momentPlaceDTO struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// maxPlacesPerMoment 是 Assets(with_members=1) places 字段的城市数量上限,
// 防止极端多地点的时刻把响应撑得过长(见设计 spec 第三节)。
const maxPlacesPerMoment = 8

// Assets returns a moment's member assets, serialized via the same asset
// shape as GET /v1/photos/assets. Query param featured=1 restricts to the
// featured (精选) subset; members are already ordered by score DESC by
// MomentStore.GetMomentAssets, and that order is preserved end-to-end because
// Search().ListAssets short-circuits on a non-nil AssetIDs filter without
// re-sorting.
//
// with_members=1 时响应形状变为 {"assets":[...], "members":[{"asset_id",
// "manual","featured"}]},供编辑 UI 同时拿到成员的来源/精选标记;不带该
// 参数时保持既有裸数组完全不变(部署窗口内旧前端仍按裸数组解析,见简报
// 歧义裁决)。
//
// GET /v1/photos/moments/:id/assets?featured=1&with_members=1
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

	if c.QueryParam("with_members") != "1" {
		return c.JSON(http.StatusOK, assets)
	}

	memberDTOs := make([]momentAssetMemberDTO, 0, len(members))
	for _, m := range members {
		memberDTOs = append(memberDTOs, momentAssetMemberDTO{
			AssetID:  m.AssetID,
			Manual:   m.Manual,
			Featured: m.Featured,
		})
	}

	places, err := h.svc.Moments().Store().PlacesByMoment(id, maxPlacesPerMoment)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	placeDTOs := make([]momentPlaceDTO, 0, len(places))
	for _, p := range places {
		placeDTOs = append(placeDTOs, momentPlaceDTO{Name: p.Name, Count: p.Count})
	}

	return c.JSON(http.StatusOK, map[string]any{"assets": assets, "members": memberDTOs, "places": placeDTOs})
}

// momentAssetsIDsRequest 是 Pin/ExcludeAssets 共用的请求体形状:一批待操作
// 的 asset id。
type momentAssetsIDsRequest struct {
	IDs []string `json:"ids"`
}

// PinAssets 把若干 asset 强制并入某时刻:落一条编辑记录并立即改成员(见
// MomentStore.PinMomentAssets),对下一轮引擎重算也生效(回放钩子见
// momentstore.go applyMomentEdits)。空 ids 视为无效请求 → 400;moment 不
// 存在或已隐藏 → 404(findMoment 统一拦,ListMoments 已过滤 hidden,故此处
// 不会触达 store 对未知 momentID 的 error 路径)。assets 表里不存在的 id
// 由 store 层静默忽略,不影响本次请求成功。
//
// POST /v1/photos/moments/:id/assets
//
//	{ "ids": ["assetId1", "assetId2", ...] }
func (h *MomentsHandler) PinAssets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	var req momentAssetsIDsRequest
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	count, err := h.svc.Moments().Store().PinMomentAssets(id, req.IDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "asset_count": count})
}

// ExcludeAssets 把若干 asset 强制剔除出某时刻(见
// MomentStore.ExcludeMomentAssets),封面重挑已在 store 层完成。空
// ids/moment 不存在或已隐藏的口径与 PinAssets 完全同形。
//
// DELETE /v1/photos/moments/:id/assets
//
//	{ "ids": ["assetId1", "assetId2", ...] }
func (h *MomentsHandler) ExcludeAssets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	var req momentAssetsIDsRequest
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	count, err := h.svc.Moments().Store().ExcludeMomentAssets(id, req.IDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "asset_count": count})
}

// Delete 隐藏某时刻(tombstone,行本身保留,见 MomentStore.HideMoment)。
// moment 不存在或已隐藏 → 404(findMoment 基于 ListMoments,已过滤
// hidden=0,故重复 DELETE 同一个 id 第二次起也是 404,是预期的幂等表现)。
// 隐藏生效后 List/Assets/CreateAlbum(export)一律 404/不再列出。
//
// DELETE /v1/photos/moments/:id
func (h *MomentsHandler) Delete(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	if err := h.svc.Moments().Store().HideMoment(id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
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
		if dto.Kind != "trip" && dto.Kind != "theme" {
			// kind 白名单:引擎(service/moments.go recomputeRecipe)只认
			// "trip"/"theme" 两种,其它 kind 会被静默 Warn 跳过(不报错)—
			// 这在推送入口这层拦掉更好:防止运维/脚本 typo(如
			// "thmee"/"Trip")悄悄把无效 recipe 写进库,重算永远不会产出、
			// 也没有任何报错提示。
			return echo.NewHTTPError(http.StatusBadRequest, "kind must be one of: trip, theme")
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

// ReorderMoments 是拖拽排序的落库入口:body 里的 ids 顺序即用户拖拽后的
// 展示顺序,事务内按序赋 sort_order(见 MomentStore.ReorderMoments)。空
// ids 视为无效请求(拖拽必然携带至少一个 id)→ 400;body 里未知的 id
// (前端列表可能略旧)在 store 层被忽略,不影响本次请求成功。
//
// PUT /v1/photos/moments/order
//
//	{ "ids": ["id1", "id2", ...] }
func (h *MomentsHandler) ReorderMoments(c echo.Context) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	if err := h.svc.Moments().Store().ReorderMoments(req.IDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// mapMomentErr maps moment-lookup errors to HTTP responses.
func mapMomentErr(err error) error {
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}
