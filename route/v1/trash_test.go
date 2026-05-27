package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

type trashStubServices struct {
	service.Services
	trash *service.TrashService
}

func (s trashStubServices) Trash() *service.TrashService { return s.trash }

// TestTrashListEmpty 验证空回收站时 List 返回 200 + JSON 数组。
func TestTrashListEmpty(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := trashStubServices{trash: service.NewTrashService(db, "/tmp/gallery", "/tmp/thumbs")}
	h := NewTrashHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/trash", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	if err := h.List(c); err != nil {
		t.Fatalf("List: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body == "" || body[0] != '[' {
		t.Fatalf("body = %q, want JSON array", body)
	}
}
