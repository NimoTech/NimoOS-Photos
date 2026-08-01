// Package uploadstore provides NimoOS-Photos's native database/sql-based
// upload.Store implementation. Column names correspond exactly to the
// o_upload_tasks table in pkg/sqlite/db.go; timestamps (created_at/updated_at)
// are written explicitly by the code — there is no GORM
// autoCreateTime/autoUpdateTime support.
package uploadstore

import (
	"database/sql"
	"errors"
	"time"

	upload "github.com/NimoTech/NimoOS-Common/upload"
)

// Store is the native database/sql-based upload.Store implementation.
type Store struct {
	db *sql.DB
}

// NewStore returns a *Store bound to the given *sql.DB (which should already be migrated).
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Column order: matches the o_upload_tasks table's column definitions.
// Table definition order:
//   id, owner_user_id, filename, target_path, relative_path,
//   size, mime, fingerprint, content_hash, upload_url,
//   uploaded_offset, status, retry_count, error, last_error_at,
//   batch_id, client_id, client_meta, created_at, updated_at, expires_at

const selectAllCols = `id,owner_user_id,filename,target_path,relative_path,` +
	`size,mime,fingerprint,content_hash,upload_url,` +
	`uploaded_offset,status,retry_count,error,last_error_at,` +
	`batch_id,client_id,client_meta,created_at,updated_at,expires_at`

// scanTask scans a row into an UploadTask using a fixed column order. The
// column order here must match selectAllCols exactly.
func scanTask(row interface {
	Scan(dest ...interface{}) error
}) (*upload.UploadTask, error) {
	var t upload.UploadTask
	err := row.Scan(
		&t.ID, &t.OwnerUserID, &t.Filename, &t.TargetPath, &t.RelativePath,
		&t.Size, &t.Mime, &t.Fingerprint, &t.ContentHash, &t.UploadURL,
		&t.Offset, &t.Status, &t.RetryCount, &t.Error, &t.LastErrorAt,
		&t.BatchID, &t.ClientID, &t.ClientMeta, &t.CreatedAt, &t.UpdatedAt, &t.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Create writes every column (including created_at/updated_at=now).
func (s *Store) Create(t *upload.UploadTask) error {
	now := time.Now().Unix()
	// Keep the caller's timestamp if already set, otherwise use now.
	createdAt := t.CreatedAt
	if createdAt == 0 {
		createdAt = now
	}
	updatedAt := t.UpdatedAt
	if updatedAt == 0 {
		updatedAt = now
	}

	_, err := s.db.Exec(
		`INSERT INTO o_upload_tasks (
			id,owner_user_id,filename,target_path,relative_path,
			size,mime,fingerprint,content_hash,upload_url,
			uploaded_offset,status,retry_count,error,last_error_at,
			batch_id,client_id,client_meta,created_at,updated_at,expires_at
		) VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?)`,
		t.ID, t.OwnerUserID, t.Filename, t.TargetPath, t.RelativePath,
		t.Size, t.Mime, t.Fingerprint, t.ContentHash, t.UploadURL,
		t.Offset, t.Status, t.RetryCount, t.Error, t.LastErrorAt,
		t.BatchID, t.ClientID, t.ClientMeta, createdAt, updatedAt, t.ExpiresAt,
	)
	return err
}

// Get looks up a task by id; returns upload.ErrNotFound when missing.
func (s *Store) Get(id string) (*upload.UploadTask, error) {
	row := s.db.QueryRow(
		`SELECT `+selectAllCols+` FROM o_upload_tasks WHERE id=?`, id,
	)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, upload.ErrNotFound
		}
		return nil, err
	}
	return t, nil
}

// ListActiveByOwner returns the given owner's active tasks
// (uploading/paused/failed), ordered by created_at DESC.
func (s *Store) ListActiveByOwner(owner string) ([]upload.UploadTask, error) {
	rows, err := s.db.Query(
		`SELECT `+selectAllCols+` FROM o_upload_tasks
		 WHERE owner_user_id=? AND status IN ('uploading','paused','failed')
		 ORDER BY created_at DESC`,
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []upload.UploadTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// ListDueForGC returns tasks with expires_at > 0 and <= now.
func (s *Store) ListDueForGC(now int64) ([]upload.UploadTask, error) {
	rows, err := s.db.Query(
		`SELECT `+selectAllCols+` FROM o_upload_tasks
		 WHERE expires_at > 0 AND expires_at <= ?`,
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []upload.UploadTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// UpdateOffset updates the uploaded byte count and renewed expiry time, and also maintains updated_at.
func (s *Store) UpdateOffset(id string, offset, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET uploaded_offset=?, expires_at=?, updated_at=? WHERE id=?`,
		offset, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// SetStatus updates the task's status and expiry time, and also maintains updated_at.
func (s *Store) SetStatus(id, status string, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET status=?, expires_at=?, updated_at=? WHERE id=?`,
		status, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// SetFailed marks the task as failed and records the error message, and also maintains updated_at.
func (s *Store) SetFailed(id, errMsg string, lastErrorAt, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET status=?, error=?, last_error_at=?, expires_at=?, updated_at=? WHERE id=?`,
		upload.UploadStatusFailed, errMsg, lastErrorAt, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// Delete physically deletes the task record. Silently succeeds for a
// nonexistent id (does not return ErrNotFound).
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM o_upload_tasks WHERE id=?`, id)
	return err
}

// Compile-time interface compliance check: ensures *Store implements upload.Store.
var _ upload.Store = (*Store)(nil)
