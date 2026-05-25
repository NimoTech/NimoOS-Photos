package v1

import (
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// TasksHandler exposes the running task list.
type TasksHandler struct{ svc service.Services }

// NewTasksHandler constructs a TasksHandler.
func NewTasksHandler(svc service.Services) *TasksHandler { return &TasksHandler{svc} }

// List returns all running / recently completed tasks.
//
// GET /v1/photos/tasks
func (h *TasksHandler) List(c echo.Context) error {
	list := []service.Task{}
	if reg := h.svc.Tasks(); reg != nil {
		got := reg.List()
		if got != nil {
			list = got
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"tasks": list})
}
