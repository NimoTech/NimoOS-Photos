package v1

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type FavoritesHandler struct {
	svc        service.Services
	galleryDir string
}

func NewFavoritesHandler(svc service.Services, galleryDir string) *FavoritesHandler {
	return &FavoritesHandler{svc: svc, galleryDir: galleryDir}
}

func (h *FavoritesHandler) Favorite(c echo.Context) error {
	favAt, err := h.svc.Favorites().Favorite(JWTUserID(c), c.Param("asset_id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"favoritedAt": favAt,
	})
}

func (h *FavoritesHandler) Unfavorite(c echo.Context) error {
	if err := h.svc.Favorites().Unfavorite(JWTUserID(c), c.Param("asset_id")); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *FavoritesHandler) ListIDs(c echo.Context) error {
	ids, err := h.svc.Favorites().ListIDs(JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if ids == nil {
		ids = []string{}
	}
	return c.JSON(http.StatusOK, ids)
}

func (h *FavoritesHandler) List(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit < 0 || limit > 500 {
		limit = 0
	}
	assets, err := h.svc.Favorites().List(JWTUserID(c), service.ListFavoritesOpts{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if assets == nil {
		assets = []service.Asset{}
	}
	return c.JSON(http.StatusOK, assets)
}

// Export — GET /v1/photos/favorites/export
// 流式 ZIP 下载。单文件失败跳过、写日志。无收藏返回 400。
func (h *FavoritesHandler) Export(c echo.Context) error {
	// 手动 token 校验（因为绕过了 JWT middleware；与 thumbnail/original/live 同一模式）
	token := c.QueryParam("token")
	if token == "" {
		return echo.NewHTTPError(http.StatusUnauthorized, "token required")
	}
	_ = token // 完整 JWT 校验由 Gateway 完成；本服务被直连时只确保非空

	userID := JWTUserID(c)
	assets, err := h.svc.Favorites().List(userID, service.ListFavoritesOpts{})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(assets) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "no favorites")
	}

	filename := fmt.Sprintf("favorites-%s.zip", time.Now().Format("2006-01-02"))
	c.Response().Header().Set("Content-Type", "application/zip")
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Response().WriteHeader(http.StatusOK)

	zw := zip.NewWriter(c.Response().Writer)
	defer zw.Close()

	usedNames := map[string]int{}
	for _, a := range assets {
		name := dedupZipEntryName(a.OriginalName, usedNames)
		if name == "" {
			name = a.ID + filepath.Ext(a.FilePath)
		}

		w, err := zw.Create(name)
		if err != nil {
			zap.L().Warn("zip create entry failed", zap.String("name", name), zap.Error(err))
			continue
		}
		f, err := os.Open(a.FilePath)
		if err != nil {
			zap.L().Warn("zip skip missing file", zap.String("path", a.FilePath), zap.Error(err))
			continue
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		if copyErr != nil {
			zap.L().Warn("zip copy failed", zap.String("path", a.FilePath), zap.Error(copyErr))
			continue
		}
		if flusher, ok := c.Response().Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return nil
}

// dedupZipEntryName 返回不与 used 中已有名字冲突的文件名。
// "photo.jpg" 第二次出现变成 "photo-2.jpg"。
func dedupZipEntryName(name string, used map[string]int) string {
	if name == "" {
		return ""
	}
	if _, exists := used[name]; !exists {
		used[name] = 1
		return name
	}
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	for {
		used[name]++
		candidate := fmt.Sprintf("%s-%d%s", base, used[name], ext)
		if _, exists := used[candidate]; !exists {
			used[candidate] = 1
			return candidate
		}
	}
}
