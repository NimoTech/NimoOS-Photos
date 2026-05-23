// Package sqlite provides database initialization and helper utilities
// for NimoOS-Photos, including sqlite-vec vector table support.
package sqlite

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // CGO SQLite3 driver
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
		`CREATE TABLE IF NOT EXISTS face_detections (
			id        TEXT PRIMARY KEY,
			asset_id  TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			bbox      TEXT NOT NULL,
			embedding BLOB NOT NULL
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
			PRIMARY KEY (album_id, asset_id)
		)`,

		// ── CLIP rowid ↔ asset_id mapping ────────────────────────────────
		`CREATE TABLE IF NOT EXISTS asset_clip_idx (
			rowid    INTEGER PRIMARY KEY,
			asset_id TEXT UNIQUE NOT NULL REFERENCES assets(id) ON DELETE CASCADE
		)`,

		// ── sqlite-vec virtual table (512-dim CLIP embeddings) ────────────
		`CREATE VIRTUAL TABLE IF NOT EXISTS clip_embeddings USING vec0(
			embedding float[512]
		)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate exec %q: %w", stmt[:min(60, len(stmt))], err)
		}
	}

	return nil
}

