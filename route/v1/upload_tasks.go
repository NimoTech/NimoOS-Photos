package v1

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	commonUpload "github.com/NimoTech/NimoOS-Common/upload"
	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/labstack/echo/v4"
)

// UploadTasksHandler 处理上传任务的列出/详情/取消接口。
type UploadTasksHandler struct {
	store      commonUpload.Store
	stagingDir string
}

// NewUploadTasksHandler 返回使用生产 stagingDir(common.StagingDir)的 handler。
func NewUploadTasksHandler(store commonUpload.Store) *UploadTasksHandler {
	return &UploadTasksHandler{store: store, stagingDir: common.StagingDir}
}

// NewUploadTasksHandlerWithStaging 返回可注入 stagingDir 的 handler(测试用)。
func NewUploadTasksHandlerWithStaging(store commonUpload.Store, stagingDir string) *UploadTasksHandler {
	return &UploadTasksHandler{store: store, stagingDir: stagingDir}
}

// ListUploads GET /v1/photos/uploads
// 返回当前用户活跃任务(uploading/paused/failed),硬编码调用 ListActiveByOwner,不读 status 参数。
func (h *UploadTasksHandler) ListUploads(c echo.Context) error {
	owner := JWTUserID(c)
	tasks, err := h.store.ListActiveByOwner(owner)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if tasks == nil {
		tasks = []commonUpload.UploadTask{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"tasks": tasks})
}

// GetUpload GET /v1/photos/uploads/:id
// 返回指定任务详情;非 owner 或不存在 → 404。
func (h *UploadTasksHandler) GetUpload(c echo.Context) error {
	owner := JWTUserID(c)
	id := c.Param("id")
	t, err := h.store.Get(id)
	if errors.Is(err, commonUpload.ErrNotFound) || (err == nil && t.OwnerUserID != owner) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, t)
}

// CancelUpload POST /v1/photos/uploads/:id/cancel
// 先校验 owner(防 IDOR),再幂等取消;取消成功则清理 staging 文件。始终返回 200。
func (h *UploadTasksHandler) CancelUpload(c echo.Context) error {
	id := c.Param("id")
	owner := JWTUserID(c)

	// IDOR 校验:先查任务,确认归属再操作。
	t, err := h.store.Get(id)
	if errors.Is(err, commonUpload.ErrNotFound) || (err == nil && t.OwnerUserID != owner) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	expires := time.Now().Unix() + commonUpload.DefaultCanceledTTLSeconds
	canceled, err := commonUpload.Cancel(h.store, id, expires)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if canceled {
		os.Remove(filepath.Join(h.stagingDir, id))         //nolint:errcheck
		os.Remove(filepath.Join(h.stagingDir, id+".info")) //nolint:errcheck
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"canceled": canceled})
}
