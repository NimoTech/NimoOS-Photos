package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// TrashHandler 处理回收站列表/恢复/永久删除。
type TrashHandler struct{ svc service.Services }

// NewTrashHandler 构造 TrashHandler。
func NewTrashHandler(svc service.Services) *TrashHandler { return &TrashHandler{svc} }

// List GET /v1/photos/trash
func (h *TrashHandler) List(c echo.Context) error {
	items, err := h.svc.Trash().ListTrash(JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if items == nil {
		items = []service.Asset{}
	}
	return c.JSON(http.StatusOK, items)
}

// Restore POST /v1/photos/trash/:id/restore
func (h *TrashHandler) Restore(c echo.Context) error {
	if err := h.svc.Trash().RestoreAsset(c.Param("id")); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// Purge DELETE /v1/photos/trash/:id
func (h *TrashHandler) Purge(c echo.Context) error {
	if err := h.svc.Trash().PurgeAsset(c.Param("id")); errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	} else if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// RestoreBatch POST /v1/photos/trash/restore  body {"ids":[...]}（ids 为空=全部恢复）
func (h *TrashHandler) RestoreBatch(c echo.Context) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	if len(req.IDs) == 0 {
		if err := h.svc.Trash().RestoreAllTrash(); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	}
	for _, id := range req.IDs {
		if err := h.svc.Trash().RestoreAsset(id); err != nil && !errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// Empty POST /v1/photos/trash/empty
func (h *TrashHandler) Empty(c echo.Context) error {
	if err := h.svc.Trash().EmptyTrash(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}
