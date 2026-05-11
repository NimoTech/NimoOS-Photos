package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/exif"
	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/pkg/thumb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MLProvider is the interface the Indexer uses for ML inference.
// *mlclient.MLClient satisfies this interface (compile-time assertion below).
type MLProvider interface {
	CLIPImageEmbed(imageData []byte) ([]float32, error)
	CLIPTextEmbed(text string) ([]float32, error)
	DetectAndRecognizeFaces(imageData []byte) ([]mlclient.FaceResult, error)
	IsReady() bool
}

// Compile-time assertion: *mlclient.MLClient must implement MLProvider.
var _ MLProvider = (*mlclient.MLClient)(nil)

// supportedExts lists the file extensions the indexer will process.
var supportedExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".heic": true,
	".webp": true,
	".mp4":  true,
	".mov":  true,
	".mkv":  true,
	".avi":  true,
}

// videoExts are extensions treated as video regardless of MIME detection.
var videoExts = map[string]bool{
	".mov": true,
	".mp4": true,
	".mkv": true,
	".avi": true,
}

// Indexer processes media files into the database with a worker pool.
type Indexer struct {
	db       *sql.DB
	ml       MLProvider
	thumbDir string
	workers  int
	queue    chan string
	seen     sync.Map // in-flight dedup: path -> struct{}
}

// NewIndexer creates a new Indexer. The queue channel is buffered to 1024 entries.
func NewIndexer(db *sql.DB, ml MLProvider, thumbDir string, workers int) *Indexer {
	return &Indexer{
		db:       db,
		ml:       ml,
		thumbDir: thumbDir,
		workers:  workers,
		queue:    make(chan string, 1024),
	}
}

// Enqueue adds path to the processing queue.
// Duplicate in-flight paths are silently dropped (only one copy processed at a time).
func (ix *Indexer) Enqueue(path string) {
	// LoadOrStore: if already in flight, skip.
	if _, loaded := ix.seen.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	select {
	case ix.queue <- path:
	default:
		// queue full — release the seen lock so it can be retried later
		ix.seen.Delete(path)
	}
}

// Start launches workers goroutines that consume the queue until ctx is cancelled.
func (ix *Indexer) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < ix.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case path, ok := <-ix.queue:
					if !ok {
						return
					}
					ix.processFile(path)
					ix.seen.Delete(path)
				}
			}
		}()
	}
	wg.Wait()
}

// QueueLen returns the number of items currently waiting in the queue.
func (ix *Indexer) QueueLen() int {
	return len(ix.queue)
}

// processFile runs the full indexing pipeline for a single file.
func (ix *Indexer) processFile(path string) {
	// 1. Read file content.
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	// 2. Compute SHA-256 checksum.
	checksum := sha256File(data)

	// 3. Skip if checksum already exists in DB with status='indexed'.
	// Records with status='pending' (e.g. left by a crash) are intentionally
	// re-processed so they can reach 'indexed' status.
	var existingID string
	err = ix.db.QueryRow(`SELECT id FROM assets WHERE checksum=? AND status='indexed'`, checksum).Scan(&existingID)
	if err == nil {
		// already fully indexed — nothing to do
		return
	}

	// 4. Detect MIME type and decide image vs. video.
	mime := http.DetectContentType(data)
	ext := strings.ToLower(filepath.Ext(path))
	isVideo := strings.HasPrefix(mime, "video/") || videoExts[ext]

	// 5. Gather metadata.
	var takenAt time.Time
	var durationMs int64
	var exifResult *exif.Result
	var keyframePath string
	var keyframeTmpDir string

	if isVideo {
		// Extract keyframe to a temp dir (cleaned up after processing).
		keyframeTmpDir, err = os.MkdirTemp("", "nimoos-kf-*")
		if err == nil {
			keyframePath, err = ffmpeg.ExtractKeyframe(path, keyframeTmpDir)
			if err != nil {
				keyframePath = ""
			}
		}
		durationMs, _ = ffmpeg.GetDurationMs(path)
	} else {
		// Parse EXIF metadata.
		f, openErr := os.Open(path)
		if openErr == nil {
			exifResult = exif.Parse(f)
			f.Close()
			if exifResult != nil && !exifResult.TakenAt.IsZero() {
				takenAt = exifResult.TakenAt
			}
		}
	}

	// 6. INSERT into assets with status='pending'.
	assetID := uuid.NewString()
	fi, _ := os.Stat(path)
	var fileSize int64
	if fi != nil {
		fileSize = fi.Size()
	}
	originalName := filepath.Base(path)

	_, err = ix.db.Exec(`
		INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
		                   taken_at, duration_ms, is_live_photo_video, status, checksum)
		VALUES(?,?,?,?,?,?,?,0,'pending',?)`,
		assetID, path, fileSize, mime, originalName,
		nullTime(takenAt), sqlNullInt64(durationMs),
		checksum,
	)
	if err != nil {
		return
	}

	// 7. INSERT EXIF metadata (images only).
	if !isVideo && exifResult != nil {
		ix.db.Exec(`
			INSERT OR IGNORE INTO asset_exif(asset_id, width, height, latitude, longitude, make, model)
			VALUES(?,?,?,?,?,?,?)`,
			assetID,
			exifResult.Width, exifResult.Height,
			exifResult.Latitude, exifResult.Longitude,
			exifResult.Make, exifResult.Model,
		)
	}

	// 8. Generate thumbnails.
	imagePath := path
	if isVideo && keyframePath != "" {
		imagePath = keyframePath
	}
	if keyframeTmpDir != "" {
		defer os.RemoveAll(keyframeTmpDir)
	}

	if imagePath != "" {
		thumb.Generate(imagePath, assetID, ix.thumbDir) //nolint:errcheck
	}

	// 9. ML inference (only when ML service is ready).
	if ix.ml.IsReady() {
		// Determine which image bytes to use for ML.
		var mlData []byte
		if isVideo && keyframePath != "" {
			mlData, _ = os.ReadFile(keyframePath)
		} else {
			mlData = data
		}

		if len(mlData) > 0 {
			// CLIP embedding.
			if vec, clipErr := ix.ml.CLIPImageEmbed(mlData); clipErr == nil {
				ix.writeClipEmbedding(assetID, vec)
			}

			// Face detection + recognition in a single request.
			if faces, faceErr := ix.ml.DetectAndRecognizeFaces(mlData); faceErr == nil {
				for _, face := range faces {
					if len(face.Embedding) != 512 {
						continue
					}
					bboxJSON, _ := json.Marshal(face.BBox)
					faceID := uuid.NewString()
					if _, err := ix.db.Exec(
						`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
						faceID, assetID, string(bboxJSON), sqlite.SerializeFloat32(face.Embedding),
					); err != nil {
						zap.L().Error("indexer: failed to insert face_detection",
							zap.String("assetID", assetID), zap.Error(err))
					}
				}
			}
		}
	}

	// 10. Mark as indexed.
	if _, err := ix.db.Exec(`
		UPDATE assets SET status='indexed', indexed_at=? WHERE id=?`,
		time.Now(), assetID,
	); err != nil {
		zap.L().Error("indexer: failed to mark asset as indexed",
			zap.String("assetID", assetID), zap.Error(err))
	}
}

// writeClipEmbedding upserts the CLIP embedding for the given asset.
func (ix *Indexer) writeClipEmbedding(assetID string, vec []float32) {
	var rowid int64
	err := ix.db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid)
	if err != nil {
		res, err2 := ix.db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
		if err2 == nil {
			rowid, _ = res.LastInsertId()
		}
	}
	if rowid > 0 {
		blob := sqlite.SerializeFloat32(vec)
		if _, err := ix.db.Exec(`INSERT OR REPLACE INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, blob); err != nil {
			zap.L().Error("indexer: failed to upsert clip_embeddings",
				zap.String("assetID", assetID), zap.Error(err))
		}
	}
}

// sha256File returns the hex-encoded SHA-256 hash of data.
func sha256File(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ScanDirectory walks dir and enqueues all supported media files.
func (ix *Indexer) ScanDirectory(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if supportedExts[ext] {
			ix.Enqueue(path)
		}
		return nil
	})
}

// ScanPending enqueues all assets currently in 'pending' status.
func (ix *Indexer) ScanPending() error {
	rows, err := ix.db.Query(`SELECT file_path FROM assets WHERE status='pending'`)
	if err != nil {
		return fmt.Errorf("indexer ScanPending: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		ix.Enqueue(path)
	}
	return rows.Err()
}

// StatusCounts returns current indexing statistics.
func (ix *Indexer) StatusCounts() IndexStatus {
	var s IndexStatus
	rows, err := ix.db.Query(
		`SELECT status, COUNT(*) FROM assets GROUP BY status`,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			var cnt int
			if rows.Scan(&status, &cnt) == nil {
				switch status {
				case "pending":
					s.Pending = cnt
				case "indexed":
					s.Indexed = cnt
				case "error":
					s.Error = cnt
				}
			}
		}
	}
	s.QueueLen = ix.QueueLen()
	return s
}

// sqlNullInt64 converts int64 to sql.NullInt64 (zero → invalid).
func sqlNullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: v}
}
