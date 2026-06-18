package v1_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	upload "github.com/NimoTech/NimoOS-Common/upload"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service/uploadstore"
	"github.com/labstack/echo/v4"
)

// openUploadTestDB 在临时目录开一个带 migrate 的 SQLite db。
func openUploadTestDB(t *testing.T) interface{ Close() error } {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// makeTask 创建最小有效任务记录。
func makeTask(id, owner, status string) *upload.UploadTask {
	now := time.Now().Unix()
	return &upload.UploadTask{
		ID:          id,
		OwnerUserID: owner,
		Filename:    id + ".jpg",
		Size:        1024,
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now + 3600,
	}
}

// TestListUploadsFiltersByOwner 验证 ListUploads 只返回请求 owner 的活跃任务。
func TestListUploadsFiltersByOwner(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)

	// owner-A 有 2 个活跃任务
	if err := store.Create(makeTask("ua1", "owner-A", upload.UploadStatusUploading)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Create(makeTask("ua2", "owner-A", upload.UploadStatusPaused)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// owner-B 有 1 个任务,不应出现在 owner-A 的响应里
	if err := store.Create(makeTask("ub1", "owner-B", upload.UploadStatusUploading)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := v1.NewUploadTasksHandler(store)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/uploads?status=active", nil)
	req.Header.Set("X-NimoOS-User-ID", "owner-A")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.ListUploads(c); err != nil {
		t.Fatalf("ListUploads returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Tasks []upload.UploadTask `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Errorf("expected 2 tasks for owner-A, got %d", len(resp.Tasks))
	}
	for _, task := range resp.Tasks {
		if task.OwnerUserID != "owner-A" {
			t.Errorf("unexpected owner in response: %s", task.OwnerUserID)
		}
	}
}

// TestGetUploadIDOR 验证 GetUpload 对不属于请求 owner 的任务返回 404(IDOR 防护)。
func TestGetUploadIDOR(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)
	if err := store.Create(makeTask("idor-task", "real-owner", upload.UploadStatusUploading)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := v1.NewUploadTasksHandler(store)
	e := echo.New()

	// 1. 不同 owner 请求 → 404
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/uploads/idor-task", nil)
	req.Header.Set("X-NimoOS-User-ID", "attacker")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("idor-task")

	err = h.GetUpload(c)
	if err == nil {
		t.Fatal("expected error for IDOR attempt, got nil")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusNotFound {
		t.Errorf("expected 404 HTTPError, got: %v", err)
	}

	// 2. 正确 owner 请求 → 200
	req2 := httptest.NewRequest(http.MethodGet, "/v1/photos/uploads/idor-task", nil)
	req2.Header.Set("X-NimoOS-User-ID", "real-owner")
	rec2 := httptest.NewRecorder()
	c2 := e.NewContext(req2, rec2)
	c2.SetParamNames("id")
	c2.SetParamValues("idor-task")

	if err := h.GetUpload(c2); err != nil {
		t.Fatalf("GetUpload correct owner: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for owner, got %d", rec2.Code)
	}

	// 3. 不存在 id → 404
	req3 := httptest.NewRequest(http.MethodGet, "/v1/photos/uploads/nonexistent", nil)
	req3.Header.Set("X-NimoOS-User-ID", "real-owner")
	rec3 := httptest.NewRecorder()
	c3 := e.NewContext(req3, rec3)
	c3.SetParamNames("id")
	c3.SetParamValues("nonexistent")

	err = h.GetUpload(c3)
	if err == nil {
		t.Fatal("expected error for nonexistent task, got nil")
	}
	he3, ok3 := err.(*echo.HTTPError)
	if !ok3 || he3.Code != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent task, got: %v", err)
	}
}

// TestCancelUploadIDOR 验证 CancelUpload 先校验 owner 防 IDOR,且幂等返回 200。
func TestCancelUploadIDOR(t *testing.T) {
	tmpStaging := t.TempDir()

	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)
	if err := store.Create(makeTask("cancel-task", "real-owner", upload.UploadStatusUploading)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := v1.NewUploadTasksHandlerWithStaging(store, tmpStaging)
	e := echo.New()

	// 1. 攻击者取消别人的任务 → 404
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/uploads/cancel-task/cancel", nil)
	req.Header.Set("X-NimoOS-User-ID", "attacker")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("cancel-task")

	err = h.CancelUpload(c)
	if err == nil {
		t.Fatal("expected error for IDOR cancel attempt, got nil")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusNotFound {
		t.Errorf("expected 404 HTTPError for IDOR cancel, got: %v", err)
	}

	// 任务状态不应因攻击者的请求而改变
	task, _ := store.Get("cancel-task")
	if task.Status != upload.UploadStatusUploading {
		t.Errorf("task status changed by IDOR attempt: %s", task.Status)
	}

	// 2. 真实 owner 取消 → 200,canceled=true
	req2 := httptest.NewRequest(http.MethodPost, "/v1/photos/uploads/cancel-task/cancel", nil)
	req2.Header.Set("X-NimoOS-User-ID", "real-owner")
	rec2 := httptest.NewRecorder()
	c2 := e.NewContext(req2, rec2)
	c2.SetParamNames("id")
	c2.SetParamValues("cancel-task")

	if err := h.CancelUpload(c2); err != nil {
		t.Fatalf("CancelUpload real owner: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec2.Code)
	}

	// 3. 同一任务再取消 → 200,幂等
	req3 := httptest.NewRequest(http.MethodPost, "/v1/photos/uploads/cancel-task/cancel", nil)
	req3.Header.Set("X-NimoOS-User-ID", "real-owner")
	rec3 := httptest.NewRecorder()
	c3 := e.NewContext(req3, rec3)
	c3.SetParamNames("id")
	c3.SetParamValues("cancel-task")

	if err := h.CancelUpload(c3); err != nil {
		t.Fatalf("CancelUpload idempotent: %v", err)
	}
	if rec3.Code != http.StatusOK {
		t.Errorf("expected 200 for idempotent cancel, got %d", rec3.Code)
	}
}
