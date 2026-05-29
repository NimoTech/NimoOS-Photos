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

// scanSearchAssets scans rows from SmartSearch: the scanAssets column list PLUS
// a trailing vec.distance column. The L2 distance over unit-length CLIP vectors
// is converted to a cosine similarity in [0,1] (sim = 1 - d²/2) and stored on
// Asset.MatchScore so the UI can show a per-result match percentage.
func scanSearchAssets(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		var distance sql.NullFloat64
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo,
			&indexedAt, &a.Status, &distance,
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
		if distance.Valid {
			sim := 1 - distance.Float64*distance.Float64/2
			if sim < 0 {
				sim = 0
			} else if sim > 1 {
				sim = 1
			}
			a.MatchScore = &sim
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// scanAssetsDetailed scans rows produced by `assets LEFT JOIN asset_exif` queries.
// Column order must match the SELECT in GetAsset exactly.
func scanAssetsDetailed(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		var width, height, iso, orientation, bitRate, rotation sql.NullInt64
		var latitude, longitude, aperture, focalLength, frameRate sql.NullFloat64
		var makeS, modelS, shutter, vcodec, acodec sql.NullString
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo,
			&indexedAt, &a.Status,
			&width, &height, &latitude, &longitude, &makeS, &modelS,
			&iso, &shutter, &aperture, &focalLength, &orientation,
			&vcodec, &acodec, &frameRate, &bitRate, &rotation,
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
		if width.Valid {
			a.Width = int(width.Int64)
		}
		if height.Valid {
			a.Height = int(height.Int64)
		}
		if latitude.Valid {
			a.Latitude = latitude.Float64
		}
		if longitude.Valid {
			a.Longitude = longitude.Float64
		}
		if makeS.Valid {
			a.Make = makeS.String
		}
		if modelS.Valid {
			a.Model = modelS.String
		}
		if iso.Valid {
			a.ISO = int(iso.Int64)
		}
		if shutter.Valid {
			a.ShutterSpeed = shutter.String
		}
		if aperture.Valid {
			a.Aperture = aperture.Float64
		}
		if focalLength.Valid {
			a.FocalLength = focalLength.Float64
		}
		if orientation.Valid {
			a.Orientation = int(orientation.Int64)
		}
		if vcodec.Valid {
			a.VideoCodec = vcodec.String
		}
		if acodec.Valid {
			a.AudioCodec = acodec.String
		}
		if frameRate.Valid {
			a.FrameRate = frameRate.Float64
		}
		if bitRate.Valid {
			a.BitRate = bitRate.Int64
		}
		if rotation.Valid {
			a.Rotation = int(rotation.Int64)
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// scanAssetsDetailedWithFav scans rows that match scanAssetsDetailed's SELECT
// list PLUS a trailing favorited_at column. Used by Timeline/GetAsset/ListAssets
// and Favorites.List where the caller wants per-user favorite state populated.
func scanAssetsDetailedWithFav(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt, favAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		var width, height, iso, orientation, bitRate, rotation sql.NullInt64
		var latitude, longitude, aperture, focalLength, frameRate sql.NullFloat64
		var makeS, modelS, shutter, vcodec, acodec sql.NullString
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo,
			&indexedAt, &a.Status,
			&width, &height, &latitude, &longitude, &makeS, &modelS,
			&iso, &shutter, &aperture, &focalLength, &orientation,
			&vcodec, &acodec, &frameRate, &bitRate, &rotation,
			&favAt,
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
		if width.Valid {
			a.Width = int(width.Int64)
		}
		if height.Valid {
			a.Height = int(height.Int64)
		}
		if latitude.Valid {
			a.Latitude = latitude.Float64
		}
		if longitude.Valid {
			a.Longitude = longitude.Float64
		}
		if makeS.Valid {
			a.Make = makeS.String
		}
		if modelS.Valid {
			a.Model = modelS.String
		}
		if iso.Valid {
			a.ISO = int(iso.Int64)
		}
		if shutter.Valid {
			a.ShutterSpeed = shutter.String
		}
		if aperture.Valid {
			a.Aperture = aperture.Float64
		}
		if focalLength.Valid {
			a.FocalLength = focalLength.Float64
		}
		if orientation.Valid {
			a.Orientation = int(orientation.Int64)
		}
		if vcodec.Valid {
			a.VideoCodec = vcodec.String
		}
		if acodec.Valid {
			a.AudioCodec = acodec.String
		}
		if frameRate.Valid {
			a.FrameRate = frameRate.Float64
		}
		if bitRate.Valid {
			a.BitRate = bitRate.Int64
		}
		if rotation.Valid {
			a.Rotation = int(rotation.Int64)
		}
		if favAt.Valid {
			t := favAt.Time
			a.FavoritedAt = &t
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
