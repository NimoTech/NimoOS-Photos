package service

import (
	"database/sql"
	"time"
)

// scanAssets reads a list of Asset from *sql.Rows.
// Column order must be: id, file_path, file_size, mime_type, original_name,
// taken_at, duration_ms, live_photo_video_id, is_live_photo_video, indexed_at, status
func scanAssets(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo,
			&indexedAt, &a.Status,
		); err != nil {
			return nil, err
		}
		if fileSize.Valid {
			a.FileSize = fileSize.Int64
		}
		if takenAt.Valid {
			t := takenAt.Time
			a.TakenAt = &t
		}
		if durationMs.Valid {
			a.DurationMs = durationMs.Int64
		}
		if indexedAt.Valid {
			t := indexedAt.Time
			a.IndexedAt = &t
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// nullTime converts a time.Time to sql.NullTime (zero value → invalid).
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Valid: true, Time: t}
}
