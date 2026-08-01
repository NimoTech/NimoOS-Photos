package uploadstore_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	upload "github.com/NimoTech/NimoOS-Common/upload"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service/uploadstore"
)

// newTask returns a minimal valid UploadTask.
func newTask(id, owner string) *upload.UploadTask {
	now := time.Now().Unix()
	return &upload.UploadTask{
		ID:          id,
		OwnerUserID: owner,
		Filename:    "test.jpg",
		Size:        1024,
		Status:      upload.UploadStatusUploading,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now + 3600,
	}
}

// TestCreateAndGet verifies Create writes, Get reads by id, and ErrNotFound mapping.
func TestCreateAndGet(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)

	task := newTask("task-001", "user-1")
	if err := store.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get("task-001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "task-001" {
		t.Errorf("ID mismatch: got %q", got.ID)
	}
	if got.OwnerUserID != "user-1" {
		t.Errorf("OwnerUserID mismatch: got %q", got.OwnerUserID)
	}
	if got.Filename != "test.jpg" {
		t.Errorf("Filename mismatch: got %q", got.Filename)
	}
	if got.Status != upload.UploadStatusUploading {
		t.Errorf("Status mismatch: got %q", got.Status)
	}
	// created_at / updated_at are explicitly written by Create, should not be zero
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should not be zero after Create")
	}
	if got.UpdatedAt == 0 {
		t.Error("UpdatedAt should not be zero after Create")
	}
}

// TestGetErrNotFound verifies a missing id returns upload.ErrNotFound (via errors.Is).
func TestGetErrNotFound(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)

	_, err = store.Get("nonexistent-id")
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}
	if !errors.Is(err, upload.ErrNotFound) {
		t.Errorf("expected upload.ErrNotFound, got: %v", err)
	}
}

// TestListActiveByOwner verifies ListActiveByOwner filters by owner and only returns active statuses.
func TestListActiveByOwner(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)

	owner1 := "owner-1"
	owner2 := "owner-2"

	// owner1: 3 active statuses
	tasks := []*upload.UploadTask{
		{ID: "t1", OwnerUserID: owner1, Status: upload.UploadStatusUploading,
			Filename: "a.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600},
		{ID: "t2", OwnerUserID: owner1, Status: upload.UploadStatusPaused,
			Filename: "b.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600},
		{ID: "t3", OwnerUserID: owner1, Status: upload.UploadStatusFailed,
			Filename: "c.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600},
		// completed/canceled should not appear in the active list
		{ID: "t4", OwnerUserID: owner1, Status: upload.UploadStatusCompleted,
			Filename: "d.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: 0},
		{ID: "t5", OwnerUserID: owner1, Status: upload.UploadStatusCanceled,
			Filename: "e.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600},
		// owner2's task should not appear in owner1's list
		{ID: "t6", OwnerUserID: owner2, Status: upload.UploadStatusUploading,
			Filename: "f.jpg", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600},
	}
	for _, tk := range tasks {
		if err := store.Create(tk); err != nil {
			t.Fatalf("Create %s: %v", tk.ID, err)
		}
	}

	list, err := store.ListActiveByOwner(owner1)
	if err != nil {
		t.Fatalf("ListActiveByOwner: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("expected 3 active tasks for owner1, got %d", len(list))
	}
	for _, item := range list {
		if item.OwnerUserID != owner1 {
			t.Errorf("unexpected owner: %s", item.OwnerUserID)
		}
		switch item.Status {
		case upload.UploadStatusUploading, upload.UploadStatusPaused, upload.UploadStatusFailed:
			// ok
		default:
			t.Errorf("unexpected status in active list: %s", item.Status)
		}
	}
}

// TestListDueForGC verifies ListDueForGC filters by expires_at (>0 AND <=now).
func TestListDueForGC(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)

	now := time.Now().Unix()
	tasks := []*upload.UploadTask{
		// already expired, should be GC'd
		{ID: "gc1", OwnerUserID: "u1", Status: upload.UploadStatusCanceled,
			Filename: "a.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: now - 100},
		// expires in the future, should not be GC'd
		{ID: "gc2", OwnerUserID: "u1", Status: upload.UploadStatusPaused,
			Filename: "b.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: now + 9999},
		// expires_at=0 (never expires, e.g. completed), should not be GC'd
		{ID: "gc3", OwnerUserID: "u1", Status: upload.UploadStatusCompleted,
			Filename: "c.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: 0},
	}
	for _, tk := range tasks {
		if err := store.Create(tk); err != nil {
			t.Fatalf("Create %s: %v", tk.ID, err)
		}
	}

	due, err := store.ListDueForGC(now)
	if err != nil {
		t.Fatalf("ListDueForGC: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("expected 1 due task, got %d", len(due))
	}
	if len(due) > 0 && due[0].ID != "gc1" {
		t.Errorf("expected gc1, got %s", due[0].ID)
	}
}

// TestCancelIdempotent verifies upload.Cancel is idempotent:
//   - Cancel on an already-canceled task → (false, nil)
//   - Cancel on an already-completed task → (false, nil)
//   - Cancel on a nonexistent task → (false, nil)
//   - Cancel on an uploading task → (true, nil), status becomes canceled afterward
func TestCancelIdempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)
	now := time.Now().Unix()

	// 1. uploading → cancel succeeds
	t1 := &upload.UploadTask{
		ID: "cancel-1", OwnerUserID: "u1", Status: upload.UploadStatusUploading,
		Filename: "a.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: now + 3600,
	}
	if err := store.Create(t1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, err := upload.Cancel(store, "cancel-1", now+60)
	if err != nil {
		t.Fatalf("Cancel uploading: %v", err)
	}
	if !ok {
		t.Error("expected Cancel to return true for uploading task")
	}
	got, _ := store.Get("cancel-1")
	if got.Status != upload.UploadStatusCanceled {
		t.Errorf("expected canceled, got %s", got.Status)
	}

	// 2. already canceled → cancel again is idempotent
	ok, err = upload.Cancel(store, "cancel-1", now+60)
	if err != nil {
		t.Fatalf("Cancel already-canceled: %v", err)
	}
	if ok {
		t.Error("expected Cancel to return false for already-canceled task")
	}

	// 3. completed → cancel does not change the status
	t2 := &upload.UploadTask{
		ID: "cancel-2", OwnerUserID: "u1", Status: upload.UploadStatusCompleted,
		Filename: "b.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: 0,
	}
	if err := store.Create(t2); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, err = upload.Cancel(store, "cancel-2", now+60)
	if err != nil {
		t.Fatalf("Cancel completed: %v", err)
	}
	if ok {
		t.Error("expected Cancel to return false for completed task")
	}

	// 4. nonexistent id → (false, nil)
	ok, err = upload.Cancel(store, "nonexistent", now+60)
	if err != nil {
		t.Fatalf("Cancel nonexistent: %v", err)
	}
	if ok {
		t.Error("expected Cancel to return false for nonexistent task")
	}
}

// TestUpdateOffset verifies UpdateOffset updates uploaded_offset and updated_at.
func TestUpdateOffset(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)
	now := time.Now().Unix()

	task := &upload.UploadTask{
		ID: "offset-1", OwnerUserID: "u1", Status: upload.UploadStatusUploading,
		Filename: "a.jpg", Size: 4096, CreatedAt: now, UpdatedAt: now, ExpiresAt: now + 3600,
	}
	if err := store.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.UpdateOffset("offset-1", 2048, now+7200); err != nil {
		t.Fatalf("UpdateOffset: %v", err)
	}

	got, err := store.Get("offset-1")
	if err != nil {
		t.Fatalf("Get after UpdateOffset: %v", err)
	}
	if got.Offset != 2048 {
		t.Errorf("Offset: expected 2048, got %d", got.Offset)
	}
	if got.ExpiresAt != now+7200 {
		t.Errorf("ExpiresAt: expected %d, got %d", now+7200, got.ExpiresAt)
	}
}

// TestDelete verifies Delete removes the record, and does not error (silently succeeds) for a nonexistent id.
func TestDelete(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	store := uploadstore.NewStore(db)
	now := time.Now().Unix()

	task := &upload.UploadTask{
		ID: "del-1", OwnerUserID: "u1", Status: upload.UploadStatusCanceled,
		Filename: "a.jpg", CreatedAt: now, UpdatedAt: now, ExpiresAt: now - 1,
	}
	if err := store.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// delete an existing record
	if err := store.Delete("del-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = store.Get("del-1")
	if !errors.Is(err, upload.ErrNotFound) {
		t.Errorf("expected ErrNotFound after Delete, got: %v", err)
	}

	// silently delete a nonexistent record — no error
	if err := store.Delete("nonexistent-xyz"); err != nil {
		t.Errorf("Delete nonexistent should be silent, got: %v", err)
	}
}
