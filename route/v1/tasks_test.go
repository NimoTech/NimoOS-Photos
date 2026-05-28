package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// stubServices implements service.Services with only Tasks() backed by a registry.
// Other methods are no-ops returning nil; this is enough for tasks handler tests.
type stubServices struct {
	service.Services // embed nil interface — panics only if unimplemented methods are called
	registry         *service.TaskRegistry
}

func (s stubServices) Tasks() *service.TaskRegistry { return s.registry }

func TestTasksHandler_Returns200WithSchema(t *testing.T) {
	reg := service.NewTaskRegistry(nil)
	reg.Upsert(service.Task{
		ID: "idx_1", Type: "index", Label: "索引照片",
		Current: 100, Total: 200, Progress: 0.5,
		Status: "running", StartedAt: time.Now(),
	})

	h := NewTasksHandler(stubServices{registry: reg})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/tasks", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.List(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Tasks []service.Task `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(body.Tasks) != 1 || body.Tasks[0].Type != "index" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestTasksHandler_EmptyList(t *testing.T) {
	reg := service.NewTaskRegistry(nil)
	h := NewTasksHandler(stubServices{registry: reg})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/tasks", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.List(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var body struct {
		Tasks []service.Task `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Tasks == nil {
		t.Fatalf("tasks should be empty array, not nil")
	}
	if len(body.Tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(body.Tasks))
	}
}
