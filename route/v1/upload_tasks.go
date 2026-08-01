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

// UploadTasksHandler handles the list/detail/cancel endpoints for upload tasks.
type UploadTasksHandler struct {
	store      commonUpload.Store
	stagingDir string
}

// NewUploadTasksHandler returns a handler using the production stagingDir (common.StagingDir).
func NewUploadTasksHandler(store commonUpload.Store) *UploadTasksHandler {
	return &UploadTasksHandler{store: store, stagingDir: common.StagingDir}
}

// NewUploadTasksHandlerWithStaging returns a handler with an injectable stagingDir (for tests).
func NewUploadTasksHandlerWithStaging(store commonUpload.Store, stagingDir string) *UploadTasksHandler {
	return &UploadTasksHandler{store: store, stagingDir: stagingDir}
}

// ListUploads GET /v1/photos/uploads
// Returns the current user's active tasks (uploading/paused/failed); hardcoded to call ListActiveByOwner, ignores the status param.
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
// Returns the given task's details; not the owner or doesn't exist → 404.
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
// Validates owner first (to prevent IDOR), then cancels idempotently; cleans up staging files on successful cancel. Always returns 200.
func (h *UploadTasksHandler) CancelUpload(c echo.Context) error {
	id := c.Param("id")
	owner := JWTUserID(c)

	// IDOR check: look up the task first and confirm ownership before acting.
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
