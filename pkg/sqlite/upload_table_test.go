package sqlite

import (
	"path/filepath"
	"testing"
)

// TestUploadTasksTable verifies migrate() created all required columns in the o_upload_tasks table.
func TestUploadTasksTable(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`PRAGMA table_info(o_upload_tasks)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info failed: %v", err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}

	if len(cols) == 0 {
		t.Fatal("o_upload_tasks table does not exist (no columns returned)")
	}

	required := []string{
		"id", "owner_user_id", "filename", "target_path", "relative_path",
		"size", "mime", "fingerprint", "content_hash", "upload_url",
		"uploaded_offset", "status", "retry_count", "error", "last_error_at",
		"batch_id", "client_id", "client_meta", "created_at", "updated_at", "expires_at",
	}
	for _, col := range required {
		if !cols[col] {
			t.Errorf("missing column: %s", col)
		}
	}

	// Verify the indexes were created (by querying sqlite_master)
	indexes := map[string]bool{}
	idxRows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='o_upload_tasks'`)
	if err != nil {
		t.Fatalf("query indexes failed: %v", err)
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes[name] = true
	}

	for _, idx := range []string{"idx_upload_owner", "idx_upload_status", "idx_upload_expires"} {
		if !indexes[idx] {
			t.Errorf("missing index: %s", idx)
		}
	}

}
