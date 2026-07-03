package service

import (
	"database/sql"
	"strings"
	"time"
)

// enrichPlaceNames fills each asset's PlaceName ("City" or "City, Country") from
// asset_geo, so the client shows and filters by city instead of falling back to
// a coordinate-derived country. Assets without a geocoded city are left blank.
// The id lookup is chunked to stay within SQLite's bound-parameter limit on large
// timelines. Best-effort: a query error leaves PlaceName unset rather than failing
// the listing.
func enrichPlaceNames(db *sql.DB, assets []Asset) {
	if len(assets) == 0 {
		return
	}
	place := make(map[string]string, len(assets))
	ids := make([]string, len(assets))
	for i := range assets {
		ids[i] = assets[i].ID
	}
	const chunk = 900
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		ph := make([]string, len(part))
		args := make([]any, len(part))
		for i, id := range part {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := db.Query(
			`SELECT asset_id, COALESCE(city,''), COALESCE(country,'') FROM asset_geo WHERE asset_id IN (`+
				strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return
		}
		for rows.Next() {
			var id, city, country string
			if err := rows.Scan(&id, &city, &country); err != nil {
				continue
			}
			if city == "" {
				continue
			}
			if country != "" {
				place[id] = city + ", " + country
			} else {
				place[id] = city
			}
		}
		rows.Close()
	}
	for i := range assets {
		if p, ok := place[assets[i].ID]; ok {
			assets[i].PlaceName = p
		}
	}
}

// scanAssets reads a list of Asset from *sql.Rows.
// Column order must be: id, file_path, file_size, mime_type, original_name,
// taken_at, duration_ms, live_photo_video_id, is_live_photo_video,
// has_ocr (EXISTS over asset_ocr), indexed_at, status
func scanAssets(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
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

// nllb-siglip 系模型的图文余弦相似度远低于 openai CLIP:噪声 ≤0.02,
// 强相关 ~0.15-0.25(本库实测首版标定,换 CLIP 模型或全量重建后需重标)。
// 展示层把 [simDisplayFloor, simDisplayCeil] 线性映射到 [0,1],
// OCR 精确命中(sim=1.0)钳到 1 不受影响。
const (
	simDisplayFloor = 0.02
	simDisplayCeil  = 0.25
)

// displayScore linearly rescales a raw cosine similarity (as produced from the
// sqlite-vec L2 distance over unit-length vectors) from [simDisplayFloor,
// simDisplayCeil] into [0,1], clamping outside that band. OCR exact hits pass
// in raw=1.0, which is above simDisplayCeil and clamps to 1 as intended.
func displayScore(raw float64) float64 {
	sim := (raw - simDisplayFloor) / (simDisplayCeil - simDisplayFloor)
	if sim < 0 {
		sim = 0
	} else if sim > 1 {
		sim = 1
	}
	return sim
}

// scanSearchAssets scans rows from SmartSearch: the scanAssets column list PLUS
// latitude, longitude (from asset_exif) and a trailing vec.distance column. The
// L2 distance over unit-length CLIP vectors is converted to a cosine similarity
// (sim = 1 - d²/2), then rescaled by displayScore into [0,1] and stored on
// Asset.MatchScore so the UI can show a per-result match percentage.
func scanSearchAssets(rows *sql.Rows) ([]Asset, error) {
	var assets []Asset
	for rows.Next() {
		var a Asset
		var takenAt, indexedAt sql.NullTime
		var fileSize, durationMs sql.NullInt64
		var lat, lng, distance sql.NullFloat64
		if err := rows.Scan(
			&a.ID, &a.FilePath, &fileSize, &a.MimeType, &a.OriginalName,
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
			&indexedAt, &a.Status, &lat, &lng, &distance,
		); err != nil {
			return nil, err
		}
		if lat.Valid {
			a.Latitude = lat.Float64
		}
		if lng.Valid {
			a.Longitude = lng.Float64
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
			sim = displayScore(sim)
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
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
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
			&takenAt, &durationMs, &a.LivePhotoVideoID, &a.IsLivePhotoVideo, &a.HasOCR,
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
