// Package uploadstore 提供 NimoOS-Photos 的原生 database/sql 版本 upload.Store 实现。
// 列名与 pkg/sqlite/db.go 中 o_upload_tasks 表严格对应;时间戳(created_at/updated_at)
// 由代码显式写入,无 GORM autoCreateTime/autoUpdateTime 支持。
package uploadstore

import (
	"database/sql"
	"errors"
	"time"

	upload "github.com/NimoTech/NimoOS-Common/upload"
)

// Store 是基于原生 database/sql 的 upload.Store 实现。
type Store struct {
	db *sql.DB
}

// NewStore 返回一个 *Store,关联给定的 *sql.DB(应已 migrate)。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// 列顺序:与 o_upload_tasks 表列定义保持一致。
// 表定义顺序:
//   id, owner_user_id, filename, target_path, relative_path,
//   size, mime, fingerprint, content_hash, upload_url,
//   uploaded_offset, status, retry_count, error, last_error_at,
//   batch_id, client_id, client_meta, created_at, updated_at, expires_at

const selectAllCols = `id,owner_user_id,filename,target_path,relative_path,` +
	`size,mime,fingerprint,content_hash,upload_url,` +
	`uploaded_offset,status,retry_count,error,last_error_at,` +
	`batch_id,client_id,client_meta,created_at,updated_at,expires_at`

// scanTask 按固定列顺序扫描一行到 UploadTask。列序须与 selectAllCols 完全一致。
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

// Create 写入全列(包含 created_at/updated_at=now)。
func (s *Store) Create(t *upload.UploadTask) error {
	now := time.Now().Unix()
	// 若调用方已设置时间戳则沿用,否则用 now
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

// Get 按 id 查询任务;缺失时返回 upload.ErrNotFound。
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

// ListActiveByOwner 返回指定 owner 的活跃任务(uploading/paused/failed),
// 按 created_at DESC 排序。
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

// ListDueForGC 返回 expires_at > 0 且 <= now 的任务。
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

// UpdateOffset 更新已上传字节数及续期过期时间,同时维护 updated_at。
func (s *Store) UpdateOffset(id string, offset, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET uploaded_offset=?, expires_at=?, updated_at=? WHERE id=?`,
		offset, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// SetStatus 更新任务状态及过期时间,同时维护 updated_at。
func (s *Store) SetStatus(id, status string, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET status=?, expires_at=?, updated_at=? WHERE id=?`,
		status, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// SetFailed 将任务标记为失败并记录错误信息,同时维护 updated_at。
func (s *Store) SetFailed(id, errMsg string, lastErrorAt, expiresAt int64) error {
	_, err := s.db.Exec(
		`UPDATE o_upload_tasks SET status=?, error=?, last_error_at=?, expires_at=?, updated_at=? WHERE id=?`,
		upload.UploadStatusFailed, errMsg, lastErrorAt, expiresAt, time.Now().Unix(), id,
	)
	return err
}

// Delete 物理删除任务记录。对不存在的 id 静默成功(不返回 ErrNotFound)。
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM o_upload_tasks WHERE id=?`, id)
	return err
}

// 编译期接口合规性检查:确保 *Store 实现 upload.Store。
var _ upload.Store = (*Store)(nil)
