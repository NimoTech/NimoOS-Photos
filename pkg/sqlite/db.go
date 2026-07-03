// Package sqlite provides database initialization and helper utilities
// for NimoOS-Photos, including sqlite-vec vector table support.
package sqlite

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // CGO SQLite3 driver

	"github.com/NimoTech/NimoOS-Photos/common"
)

func init() {
	// Register the sqlite-vec extension so that vec0 virtual tables are available.
	sqlite_vec.Auto()
}

// SerializeFloat32 encodes a []float32 slice into a little-endian BLOB
// compatible with sqlite-vec's expected binary embedding format.
func SerializeFloat32(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		bits := math.Float32bits(f)
		binary.LittleEndian.PutUint32(buf[i*4:], bits)
	}
	return buf
}

// DeserializeFloat32 decodes a little-endian BLOB back into a []float32 slice.
func DeserializeFloat32(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	result := make([]float32, len(b)/4)
	for i := range result {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		result[i] = math.Float32frombits(bits)
	}
	return result
}

// Open opens (or creates) a SQLite database at the given path, enables
// WAL mode and foreign keys, and runs the full schema migration.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite.Open ping: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite.Open migrate: %w", err)
	}

	return db, nil
}

// migrate creates all tables, indexes, and virtual tables if they do not
// already exist. It is idempotent and safe to call on every startup.
func migrate(db *sql.DB) error {
	// CLIP 维度迁移:换模型导致维度变化时,旧 vec0 表必须整表重建
	// (vec0 不支持改维度)。asset_clip_idx 一并清空,向量由启动后的
	// 自动全量重建 / Embedder 补跑重新生成。
	if err := migrateClipDim(db); err != nil {
		return err
	}

	statements := []string{
		// ── Core asset table ──────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT REFERENCES assets(id),
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_assets_checksum  ON assets(checksum)`,
		`CREATE INDEX IF NOT EXISTS idx_assets_taken_at  ON assets(taken_at)`,
		`CREATE INDEX IF NOT EXISTS idx_assets_status    ON assets(status)`,
		`CREATE INDEX IF NOT EXISTS idx_assets_livevideo ON assets(is_live_photo_video)`,

		// ── EXIF metadata ─────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_exif (
			asset_id  TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			width     INTEGER,
			height    INTEGER,
			latitude  REAL,
			longitude REAL,
			make      TEXT,
			model     TEXT
		)`,

		// ── Face detections ───────────────────────────────────────────────
		// excluded=1 标记用户从某 person 移出的脸，从此不参与聚类/吸附/列表。
		`CREATE TABLE IF NOT EXISTS face_detections (
			id        TEXT PRIMARY KEY,
			asset_id  TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			bbox      TEXT NOT NULL,
			embedding BLOB NOT NULL,
			excluded  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_faces_asset ON face_detections(asset_id)`,

		// ── Persons (face clusters) ───────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS persons (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL DEFAULT '',
			cover_asset_id TEXT REFERENCES assets(id) ON DELETE SET NULL
		)`,

		// ── Face→Person mapping ───────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS face_person (
			face_id   TEXT PRIMARY KEY REFERENCES face_detections(id) ON DELETE CASCADE,
			person_id TEXT REFERENCES persons(id) ON DELETE SET NULL
		)`,

		// ── Albums ────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS albums (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL,
			created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			cover_asset_id TEXT REFERENCES assets(id) ON DELETE SET NULL
		)`,

		// ── Album membership ──────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS album_assets (
			album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
			asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			added_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			position INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (album_id, asset_id)
		)`,

		// ── CLIP rowid ↔ asset_id mapping ────────────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_clip_idx (
			rowid    INTEGER PRIMARY KEY,
			asset_id TEXT UNIQUE NOT NULL REFERENCES assets(id) ON DELETE CASCADE
		)`,

		// ── sqlite-vec virtual table (CLIP embeddings, 维度跟随 common.CLIPDim) ──
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS clip_embeddings USING vec0(
			embedding float[%d]
		)`, common.CLIPDim),

		// ── Favorites (per-user, time-indexed) ────────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_favorites (
			user_id      TEXT NOT NULL DEFAULT 'default',
			asset_id     TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			favorited_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, asset_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fav_user_time ON asset_favorites(user_id, favorited_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_fav_asset     ON asset_favorites(asset_id)`,

		// ── Asset views (per-user open counter) ───────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_views (
			user_id        TEXT NOT NULL DEFAULT 'default',
			asset_id       TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			view_count     INTEGER NOT NULL DEFAULT 0,
			last_viewed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, asset_id)
		)`,

		// ── Merge rejections (rejected merge-suggestion pairs, (min,max) unique) ──
		`CREATE TABLE IF NOT EXISTS merge_rejections (
			person_a    TEXT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
			person_b    TEXT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
			rejected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (person_a, person_b),
			CHECK (person_a < person_b)
		)`,

		// ── Geo location per asset ────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_geo (
			asset_id    TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			city_id     INTEGER,
			city        TEXT,
			country     TEXT,
			region      TEXT,
			admin1      TEXT,
			lat         REAL,
			lon         REAL,
			geocoded_at DATETIME
		)`,

		// ── Place cover overrides (per-user custom cover asset) ───────────────
		`CREATE TABLE IF NOT EXISTS place_cover_overrides (
			user_id   TEXT NOT NULL,
			place_key INTEGER NOT NULL,
			asset_id  TEXT NOT NULL,
			PRIMARY KEY (user_id, place_key)
		)`,

		// ── Spot name overrides (per-user custom name for an auto-detected spot) ─
		`CREATE TABLE IF NOT EXISTS spot_name_overrides (
			user_id  TEXT NOT NULL,
			spot_key TEXT NOT NULL,
			name     TEXT NOT NULL,
			PRIMARY KEY (user_id, spot_key)
		)`,

		// ── Smart Views ───────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS smart_views (
			id            TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			description   TEXT,
			conds_raw     TEXT NOT NULL,
			conds_parsed  TEXT NOT NULL,
			threshold     INTEGER NOT NULL DEFAULT 70,
			live          INTEGER NOT NULL DEFAULT 1,
			include_videos INTEGER NOT NULL DEFAULT 0,
			notify_weekly INTEGER NOT NULL DEFAULT 0,
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			evaluated_at  DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS smart_view_matches (
			smart_view_id TEXT NOT NULL REFERENCES smart_views(id) ON DELETE CASCADE,
			asset_id      TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			match_score   REAL NOT NULL,
			matched_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (smart_view_id, asset_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_svm_sv_score ON smart_view_matches(smart_view_id, match_score)`,
		`CREATE INDEX IF NOT EXISTS idx_svm_matched  ON smart_view_matches(smart_view_id, matched_at)`,
		// ── OCR text per asset ────────────────────────────────────────────────
		// One row per OCR'd asset. text='' means "OCR ran, nothing recognized",
		// which lets the backfill distinguish done-but-empty from not-yet-done.
		// Matching uses instr(lower(text), ...) — FTS5 is not compiled into the
		// bundled mattn/go-sqlite3 build, and per-asset text is small enough
		// that a LIKE-style scan is fine at gallery scale.
		`CREATE TABLE IF NOT EXISTS asset_ocr (
			asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			text     TEXT NOT NULL DEFAULT '',
			ocr_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS smart_view_activity (
			id            TEXT PRIMARY KEY,
			smart_view_id TEXT NOT NULL REFERENCES smart_views(id) ON DELETE CASCADE,
			event_type    TEXT NOT NULL,
			detail        TEXT,
			asset_ids     TEXT,
			occurred_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		// ── 键值元数据（如 index_last_rebuilt）──────────────────────────────
		`CREATE TABLE IF NOT EXISTS photos_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		// ── 可恢复上传任务表（与 Common upload.UploadTask gorm column 对应）────
		`CREATE TABLE IF NOT EXISTS o_upload_tasks (
			id TEXT PRIMARY KEY,
			owner_user_id TEXT,
			filename TEXT, target_path TEXT, relative_path TEXT,
			size INTEGER, mime TEXT,
			fingerprint TEXT, content_hash TEXT,
			upload_url TEXT, uploaded_offset INTEGER DEFAULT 0,
			status TEXT, retry_count INTEGER DEFAULT 0,
			error TEXT, last_error_at INTEGER DEFAULT 0,
			batch_id TEXT, client_id TEXT, client_meta TEXT,
			created_at INTEGER DEFAULT 0, updated_at INTEGER DEFAULT 0,
			expires_at INTEGER DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_owner   ON o_upload_tasks(owner_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_status  ON o_upload_tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_expires ON o_upload_tasks(expires_at)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate exec %q: %w", stmt[:min(60, len(stmt))], err)
		}
	}

	// Idempotent column expansion: extend asset_exif to hold full EXIF + video metadata.
	newCols := []struct {
		name string
		decl string
	}{
		{"iso", "INTEGER"},
		{"shutter_speed", "TEXT"},
		{"aperture", "REAL"},
		{"focal_length", "REAL"},
		{"orientation", "INTEGER"},
		{"video_codec", "TEXT"},
		{"audio_codec", "TEXT"},
		{"frame_rate", "REAL"},
		{"bit_rate", "INTEGER"},
		{"rotation", "INTEGER"},
	}

	existing := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(asset_exif)`)
	if err != nil {
		return fmt.Errorf("migrate pragma asset_exif: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("migrate pragma scan: %w", err)
		}
		existing[name] = true
	}
	rows.Close()

	for _, col := range newCols {
		if existing[col.name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE asset_exif ADD COLUMN %s %s", col.name, col.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate add column %s: %w", col.name, err)
		}
	}

	// Idempotent column expansion: assets.offline tracks whether the asset's
	// source file currently lives on a removable drive that's unplugged (see
	// MountGuard in service/mountguard.go). Same PRAGMA-based idempotent-add
	// pattern as the asset_exif columns above (kept separate from the
	// duplicate-column-error `alters` slice below, per design).
	assetsNewCols := []struct {
		name string
		decl string
	}{
		{"offline", "INTEGER NOT NULL DEFAULT 0"},
	}
	assetsExisting := map[string]bool{}
	aRows, err := db.Query(`PRAGMA table_info(assets)`)
	if err != nil {
		return fmt.Errorf("migrate pragma assets: %w", err)
	}
	for aRows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := aRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			aRows.Close()
			return fmt.Errorf("migrate pragma scan assets: %w", err)
		}
		assetsExisting[name] = true
	}
	aRows.Close()
	for _, col := range assetsNewCols {
		if assetsExisting[col.name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE assets ADD COLUMN %s %s", col.name, col.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate add column assets.%s: %w", col.name, err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_assets_offline ON assets(offline)`); err != nil {
		return fmt.Errorf("migrate index assets_offline: %w", err)
	}

	// ── Idempotent column migration: legacy DBs created with CREATE TABLE IF NOT EXISTS
	//    won't have new columns; ALTER TABLE ADD COLUMN fills them in.
	//    SQLite raises "duplicate column" when the column already exists — ignore it.
	alters := []string{
		`ALTER TABLE assets ADD COLUMN deleted_at DATETIME`,
		`ALTER TABLE assets ADD COLUMN original_path TEXT`,
		`ALTER TABLE persons ADD COLUMN cover_face_id TEXT`,
		`ALTER TABLE persons ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE persons ADD COLUMN relation TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE persons ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE persons ADD COLUMN confidence REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE persons ADD COLUMN centroid BLOB`,
		`ALTER TABLE persons ADD COLUMN created_at DATETIME`,
		`ALTER TABLE persons ADD COLUMN updated_at DATETIME`,
		`ALTER TABLE album_assets ADD COLUMN position INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE face_detections ADD COLUMN excluded INTEGER NOT NULL DEFAULT 0`,
		// OCR document-ness signals: coverage = text-box area / image area in
		// [0,1] (NULL until the OCR backfill recomputes it from box geometry);
		// line_count = recognized lines kept after the confidence filter.
		`ALTER TABLE asset_ocr ADD COLUMN coverage REAL`,
		`ALTER TABLE asset_ocr ADD COLUMN line_count INTEGER`,
		// cover_locked=1 means the user has pinned a specific cover face; auto-recompute skips cover update.
		`ALTER TABLE persons ADD COLUMN cover_locked INTEGER NOT NULL DEFAULT 0`,
		// hero_asset_id: user-chosen hero/background photo for this person (must have a valid face).
		`ALTER TABLE persons ADD COLUMN hero_asset_id TEXT`,
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate alter: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_assets_deleted_at ON assets(deleted_at)`); err != nil {
		return fmt.Errorf("migrate index deleted_at: %w", err)
	}

	// Derive line_count for legacy asset_ocr rows from the stored text so the
	// OCR-category line threshold applies immediately, before the coverage
	// re-OCR backfill reaches them. Idempotent: only fills NULLs.
	if _, err := db.Exec(`
		UPDATE asset_ocr SET line_count = CASE
			WHEN text = '' THEN 0
			ELSE length(text) - length(replace(text, char(10), '')) + 1
		END
		WHERE line_count IS NULL`); err != nil {
		return fmt.Errorf("migrate backfill asset_ocr.line_count: %w", err)
	}

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_album_assets_position ON album_assets(album_id, position)`); err != nil {
		return fmt.Errorf("migrate index album_assets_position: %w", err)
	}

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_asset_geo_city ON asset_geo(city_id)`); err != nil {
		return fmt.Errorf("migrate index asset_geo_city: %w", err)
	}

	// Backfill position: only runs when an album has >=2 rows all at position=0,
	// which distinguishes legacy unbackfilled rows from a freshly-created album.
	// Idempotent: after backfill MAX(position) > 0, so HAVING never fires again.
	// NOTE: AddAsset/ReorderAssets now write distinct positions, so any album
	// where MAX(position) > 0 is treated as already backfilled. The HAVING guard
	// remains as a safety net for legacy DBs and is harmless on re-run.
	if _, err := db.Exec(`
		WITH targets AS (
			SELECT album_id FROM album_assets
			GROUP BY album_id
			HAVING COUNT(*) >= 2 AND MIN(position) = 0 AND MAX(position) = 0
		),
		ordered AS (
			SELECT album_id, asset_id,
			       ROW_NUMBER() OVER (PARTITION BY album_id ORDER BY added_at, asset_id) - 1 AS pos
			FROM album_assets WHERE album_id IN (SELECT album_id FROM targets)
		)
		UPDATE album_assets SET position = (
			SELECT pos FROM ordered o
			WHERE o.album_id = album_assets.album_id AND o.asset_id = album_assets.asset_id
		)
		WHERE album_id IN (SELECT album_id FROM targets)
	`); err != nil {
		return fmt.Errorf("migrate backfill album_assets.position: %w", err)
	}

	if err := regeocodeIfStale(db); err != nil {
		return err
	}

	return nil
}

// migrateClipDim 检查已存在的 clip_embeddings 表维度是否等于 common.CLIPDim,
// 不符则 DROP(vec0 影子表随之删除)并清空 asset_clip_idx。表不存在时为 no-op。
func migrateClipDim(db *sql.DB) error {
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='clip_embeddings'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil // 新库,走正常建表路径
	}
	if err != nil {
		return fmt.Errorf("migrateClipDim read ddl: %w", err)
	}
	if strings.Contains(ddl, fmt.Sprintf("float[%d]", common.CLIPDim)) {
		return nil // 维度已正确
	}
	if _, err := db.Exec(`DROP TABLE clip_embeddings`); err != nil {
		return fmt.Errorf("migrateClipDim drop: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM asset_clip_idx`); err != nil {
		// asset_clip_idx 可能尚未创建(极老库),忽略 no such table
		if !strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("migrateClipDim clear idx: %w", err)
		}
	}
	return nil
}

// geoGazVersion is bumped whenever the embedded gazetteer or the reverse-geocode
// algorithm changes in a way that alters which city a coordinate resolves to.
// v1: metro-snap + coarser gazetteer (drop PPLX & capital-swallowed sub-divisions),
//
//	so dense metros (e.g. Hong Kong, Macau) resolve to one city instead of many.
const geoGazVersion = 1

// regeocodeIfStale clears asset_geo when the gazetteer version stored in the DB
// is older than geoGazVersion, so the GeoService scheduler re-geocodes every
// asset against the current gazetteer. asset_geo is fully derived from
// asset_exif lat/lon, so clearing it is safe and self-healing.
func regeocodeIfStale(db *sql.DB) error {
	var stored int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
		return fmt.Errorf("migrate read user_version: %w", err)
	}
	if stored >= geoGazVersion {
		return nil
	}
	if _, err := db.Exec(`DELETE FROM asset_geo`); err != nil {
		return fmt.Errorf("migrate clear asset_geo for re-geocode: %w", err)
	}
	// PRAGMA user_version does not accept a bound parameter.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, geoGazVersion)); err != nil {
		return fmt.Errorf("migrate set user_version: %w", err)
	}
	return nil
}
