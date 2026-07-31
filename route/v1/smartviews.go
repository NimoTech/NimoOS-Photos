package v1

import (
	"crypto/ecdsa"
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/utils/jwt"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// SmartViewsHandler handles smart view CRUD, preview, and export endpoints.
type SmartViewsHandler struct {
	svc service.Services
	// runtimePath 供 ExportZip 的 query-token JWT 校验取公钥用（同
	// AlbumsHandler/FavoritesHandler 的既有约定）；空串表示测试/单机直连
	// 场景，跳过真实 JWT 校验。
	runtimePath string
}

// NewSmartViewsHandler constructs a SmartViewsHandler.
func NewSmartViewsHandler(svc service.Services, runtimePath string) *SmartViewsHandler {
	return &SmartViewsHandler{svc: svc, runtimePath: runtimePath}
}

// RegisterSmartViewRoutes registers all smart-view routes on the given group.
func RegisterSmartViewRoutes(g *echo.Group, h *SmartViewsHandler) {
	g.GET("/smart-views", h.List)
	g.POST("/smart-views", h.Create)
	g.POST("/smart-views/preview", h.Preview)
	g.GET("/smart-views/:id", h.Get)
	g.PUT("/smart-views/:id", h.Update)
	g.DELETE("/smart-views/:id", h.Delete)
	g.POST("/smart-views/:id/duplicate", h.Duplicate)
	g.GET("/smart-views/:id/assets", h.Assets)
	g.POST("/smart-views/:id/assets", h.PinAssets)
	g.POST("/smart-views/:id/assets/remove", h.RemoveAssets)
	g.POST("/smart-views/:id/assets/restore", h.RestoreAssets)
	g.GET("/smart-views/:id/excluded", h.Excluded)
	g.GET("/smart-views/:id/activity", h.Activity)
	// 既有 POST /export(format=zip|album)保持不动，向后兼容。
	g.POST("/smart-views/:id/export", h.Export)
	// 新增 GET+token 的 ZIP 直下载端点，修 UI window.location.href 断链：
	// 浏览器导航发不出 Authorization 头，POST-only 路由也接不住 GET，旧链路
	// 会被 JWT 中间件 401。镜像 albums.go AlbumsHandler.Export 的实现形状。
	g.GET("/smart-views/:id/export", h.ExportZip)
	g.POST("/smart-views/from-album", h.FromAlbum)
}

// svAssetIDsReq 是钉住/移除/恢复三个写接口共用的请求体。
type svAssetIDsReq struct {
	AssetIDs []string `json:"assetIds"`
}

// PinAssets 把指定资产钉进视图,返回本次实际发生状态变化的数量。
func (h *SmartViewsHandler) PinAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	added, err := h.svc.SmartViews().PinAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"added": added})
}

// RemoveAssets 分层移除:钉住行取消钉住,自动行置为排除。
func (h *SmartViewsHandler) RemoveAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	unpinned, excluded, err := h.svc.SmartViews().RemoveAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"unpinned": unpinned, "excluded": excluded})
}

// RestoreAssets 恢复被排除的资产,使其重新参与视图匹配。
func (h *SmartViewsHandler) RestoreAssets(c echo.Context) error {
	var req svAssetIDsReq
	if err := c.Bind(&req); err != nil || len(req.AssetIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "assetIds is required")
	}
	restored, err := h.svc.SmartViews().RestoreAssets(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"restored": restored})
}

// Excluded 返回视图的排除清单(供详情页折叠区展示)。
func (h *SmartViewsHandler) Excluded(c echo.Context) error {
	assets, err := h.svc.SmartViews().ExcludedAssets(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

func (h *SmartViewsHandler) List(c echo.Context) error {
	list, err := h.svc.SmartViews().List()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, list)
}

func (h *SmartViewsHandler) Create(c echo.Context) error {
	var in service.SmartViewInput
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	sv, err := h.svc.SmartViews().Create(in)
	if errors.Is(err, service.ErrInvalidInput) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid input")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Get(c echo.Context) error {
	sv, err := h.svc.SmartViews().Get(c.Param("id"))
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Update(c echo.Context) error {
	var p service.SmartViewPatch
	if err := c.Bind(&p); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}
	sv, err := h.svc.SmartViews().Update(c.Param("id"), p)
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Delete(c echo.Context) error {
	if err := h.svc.SmartViews().Delete(c.Param("id")); err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *SmartViewsHandler) Duplicate(c echo.Context) error {
	sv, err := h.svc.SmartViews().Duplicate(c.Param("id"))
	if err == service.ErrNotFound {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Assets(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	recent := c.QueryParam("recent") == "true"
	assets, err := h.svc.SmartViews().MatchedAssets(c.Param("id"), limit, offset, recent, JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

func (h *SmartViewsHandler) Activity(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	acts, err := h.svc.SmartViews().Activity(c.Param("id"), limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, acts)
}

func (h *SmartViewsHandler) Preview(c echo.Context) error {
	var req struct {
		CondsRaw      []string `json:"condsRaw"`
		Description   string   `json:"description"`
		Threshold     int      `json:"threshold"`
		IncludeVideos bool     `json:"includeVideos"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad payload")
	}
	count, ids, thresholdActive, err := h.svc.SmartViews().Preview(req.CondsRaw, req.Description, req.Threshold, req.IncludeVideos)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"count": count, "seeds": ids, "thresholdActive": thresholdActive})
}

// fromAlbumReq 是"手动相册→智能相册"转换端点的请求体,字段名对齐库内
// camelCase 惯例(与响应体 albumId 等字段一致)。
type fromAlbumReq struct {
	AlbumID       string   `json:"albumId"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Conds         []string `json:"conds"`
	Threshold     int      `json:"threshold"`
	IncludeVideos bool     `json:"includeVideos"`
}

// FromAlbum 把手动相册原地变身为智能相册:既有成员全部锁定为 pin,原相册
// 随之删除,Nimo 按主题持续吸入新照片。
func (h *SmartViewsHandler) FromAlbum(c echo.Context) error {
	var req fromAlbumReq
	if err := c.Bind(&req); err != nil || req.AlbumID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "albumId is required")
	}
	sv, err := h.svc.SmartViews().ConvertFromAlbum(service.ConvertFromAlbumInput{
		AlbumID:       req.AlbumID,
		Name:          req.Name,
		Description:   req.Description,
		CondsRaw:      req.Conds,
		Threshold:     req.Threshold,
		IncludeVideos: req.IncludeVideos,
	})
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "album not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sv)
}

func (h *SmartViewsHandler) Export(c echo.Context) error {
	id := c.Param("id")
	format := c.QueryParam("format")
	if format == "" {
		var req struct{ Format string `json:"format"` }
		_ = c.Bind(&req)
		format = req.Format
	}
	switch format {
	case "album":
		albumID, err := h.svc.SmartViews().ExportAsAlbum(id)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]string{"albumId": albumID})
	case "zip":
		if err := h.svc.SmartViews().ExportZip(c.Response(), id); err != nil {
			if errors.Is(err, service.ErrInvalidInput) {
				return echo.NewHTTPError(http.StatusBadRequest, "no matches to export")
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return nil
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported format")
	}
}

// ExportZip — GET /v1/photos/smart-views/:id/export?token=<jwt>
//
// 与 albums.go AlbumsHandler.Export 同款的浏览器直下载入口：修复既有 UI 断链
// ——PhotosSmartViewDetail.vue 的 runExport('zip') 用 window.location.href
// 触发下载,浏览器导航发不出 Authorization 头,而旧的 POST /export 既没注册
// GET,也不在 router.go 的 mediaGetSkip 白名单里,导致请求在 JWT 中间件就被
// 拦成 401。这里改用 query token 自行校验(runtimePath=="" 时是测试/单机
// 直连场景,跳过真实校验,同 Albums/Favorites 的既有约定),流式落地复用
// service.SmartViewService.ExportZip(与旧 POST /export?format=zip 分支完全
// 同一份实现,行为不变)。既有 POST 路由保持不动,两者并存、互不影响。
func (h *SmartViewsHandler) ExportZip(c echo.Context) error {
	token := c.QueryParam("token")
	if token == "" {
		return echo.NewHTTPError(http.StatusUnauthorized, "token required")
	}
	if h.runtimePath != "" {
		valid, _, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) {
			return external.GetPublicKey(h.runtimePath)
		})
		if err != nil || !valid {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
	}

	id := c.Param("id")
	if _, err := h.svc.SmartViews().Get(id); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	if err := h.svc.SmartViews().ExportZip(c.Response(), id); err != nil {
		if errors.Is(err, service.ErrInvalidInput) {
			return echo.NewHTTPError(http.StatusBadRequest, "no matches to export")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return nil
}
