package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/exif"
	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/pkg/thumb"
	"github.com/google/uuid"
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

// ingestQueueItem carries a file path and its optional batch association
// through the worker queue so that noteResultWithBatch can update the correct
// batch slot.
type ingestQueueItem struct {
	path    string
	batchID string // "" for watcher / ScanDirectory paths
}

// Indexer processes media files into the database with a worker pool.
type Indexer struct {
	db       *sql.DB
	ml       MLProvider
	thumbDir string
	workers  int
	queue    chan ingestQueueItem
	seen     sync.Map // in-flight dedup: path -> struct{}
	taskReg  *TaskRegistry
	ingest   *ingestTracker // aggregates Enqueue/processFile into a single rolling task
}

// defaultIngestIdleTimeout is the quiet period after which ingestTracker
// publishes the final "done" task and resets itself for the next batch.
const defaultIngestIdleTimeout = 6 * time.Second

// taskCleanupDelay is how long a completed task stays in the registry before
// being removed. Shared by ingestTracker, ScanDirectory, and FaceService so
// the UI has a consistent window to display the done state.
const taskCleanupDelay = 6 * time.Second

// ingestBatch tracks the progress of a single logical batch of files.
// The empty-string batchID ("") is the legacy singleton slot used by watcher
// and ScanDirectory; named batches come from multi-select TUS uploads.
type ingestBatch struct {
	id         string // = batchID (map key); taskID is a separate auto-generated ID
	taskID     string // "ingest_<unixnano>"
	fixedTotal bool   // true: total was set by caller (batchTotal > 0), not incremented per-enqueue
	enqueued   int64  // total number of noteEnqueueWithBatch calls for this batch (overrun detection)
	total      int64
	current    int64
	failed     int64
	startedAt  time.Time
	idleTimer  *time.Timer
}

// ingestTracker aggregates TUS/watcher-driven Enqueue calls into rolling
// type="index" Tasks, one per batchID. The empty batchID ("") maintains the
// original idle-based singleton behaviour; named batches are independent and
// each own their own task ID and timer.
type ingestTracker struct {
	mu          sync.Mutex
	reg         *TaskRegistry
	batches     map[string]*ingestBatch
	idleAfter   time.Duration
	onBatchDone func() // optional callback invoked when any batch reaches done
}

func newIngestTracker() *ingestTracker {
	return &ingestTracker{
		idleAfter: defaultIngestIdleTimeout,
		batches:   make(map[string]*ingestBatch),
	}
}

// noteEnqueue is the legacy entry point (batchID="", batchTotal=0).
func (t *ingestTracker) noteEnqueue() {
	t.noteEnqueueWithBatch("", 0)
}

// noteEnqueueWithBatch registers one new file entering the queue for the given
// batch. If batchTotal > 0 the task total is fixed from the start (only the
// first call establishes the declared size). Subsequent calls for the same
// fixedTotal batch do not change total unless enqueued count exceeds the
// declared total (front-end over-send tolerance: total is extended by 1 per
// extra file and a warning is printed).
func (t *ingestTracker) noteEnqueueWithBatch(batchID string, batchTotal int64) {
	if t == nil || t.reg == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	b, exists := t.batches[batchID]
	if !exists {
		b = &ingestBatch{
			id:        batchID,
			taskID:    fmt.Sprintf("ingest_%d", time.Now().UnixNano()),
			startedAt: time.Now(),
		}
		if batchTotal > 0 {
			b.fixedTotal = true
			b.total = batchTotal
		} else {
			// batchTotal == 0: accumulate mode (legacy "" slot behaviour).
			b.total = 1
		}
		b.enqueued = 1
		t.batches[batchID] = b
	} else {
		b.enqueued++
		if b.fixedTotal {
			// Total is fixed. Only extend it when more files are enqueued than declared.
			if b.enqueued > b.total {
				fmt.Fprintf(os.Stderr,
					"[ingestTracker] batch %q: received extra enqueue beyond declared total %d; extending\n",
					batchID, b.total)
				b.total++
			}
			// Within the declared total: do not modify total.
		} else {
			// Accumulate mode: each enqueue increments the total.
			b.total++
		}
	}

	// Cancel any pending idle timer so a new arrival keeps the task alive.
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	t.publishRunningLocked(b)
}

// noteResult is the legacy entry point (batchID="").
func (t *ingestTracker) noteResult(success bool) {
	t.noteResultWithBatch("", success)
}

// noteResultWithBatch records one completed file for the given batch.
func (t *ingestTracker) noteResultWithBatch(batchID string, success bool) {
	if t == nil || t.reg == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	b, exists := t.batches[batchID]
	if !exists {
		return
	}
	b.current++
	if !success {
		b.failed++
	}
	t.publishRunningLocked(b)
	if b.current >= b.total {
		if b.idleTimer != nil {
			b.idleTimer.Stop()
		}
		b.idleTimer = time.AfterFunc(t.idleAfter, func() { t.onIdle(batchID) })
	}
}

// publishRunningLocked publishes a running task update for b. Must be called
// with t.mu held.
func (t *ingestTracker) publishRunningLocked(b *ingestBatch) {
	progress := 0.0
	if b.total > 0 {
		progress = float64(b.current) / float64(b.total)
	}
	t.reg.Upsert(Task{
		ID:        b.taskID,
		Type:      "index",
		Label:     "索引照片",
		Current:   b.current,
		Total:     b.total,
		Progress:  progress,
		Status:    "running",
		StartedAt: b.startedAt,
	})
}

// onIdle fires after the per-batch idle timer expires; publishes the final
// "done" task event and schedules cleanup of the batch slot.
func (t *ingestTracker) onIdle(batchID string) {
	t.mu.Lock()
	b, exists := t.batches[batchID]
	if !exists || b.current < b.total {
		t.mu.Unlock()
		return
	}
	label := "索引照片"
	if b.failed > 0 {
		label = fmt.Sprintf("索引照片（失败 %d 张）", b.failed)
	}
	taskID := b.taskID
	final := Task{
		ID:        taskID,
		Type:      "index",
		Label:     label,
		Current:   b.current,
		Total:     b.total,
		Progress:  1,
		Status:    "done",
		StartedAt: b.startedAt,
	}
	b.idleTimer = nil
	reg := t.reg
	cb := t.onBatchDone
	t.mu.Unlock()

	reg.Upsert(final)
	if cb != nil {
		cb()
	}
	go func() {
		time.Sleep(taskCleanupDelay)
		t.mu.Lock()
		delete(t.batches, batchID)
		t.mu.Unlock()
		reg.Remove(taskID)
	}()
}

// NewIndexer creates a new Indexer. The queue channel is buffered to 1024 entries.
func NewIndexer(db *sql.DB, ml MLProvider, thumbDir string, workers int) *Indexer {
	return &Indexer{
		db:       db,
		ml:       ml,
		thumbDir: thumbDir,
		workers:  workers,
		queue:    make(chan ingestQueueItem, 1024),
		ingest:   newIngestTracker(),
	}
}

// SetTaskRegistry injects a TaskRegistry so ScanDirectory can report progress.
// Call this after construction (e.g. from NewService) before any scans begin.
// It also wires the registry into the ingestTracker for Enqueue-driven tasks.
func (ix *Indexer) SetTaskRegistry(reg *TaskRegistry) {
	ix.taskReg = reg
	if ix.ingest != nil {
		ix.ingest.reg = reg
	}
}

// SetIngestIdleTimeout overrides the quiet period after which ingestTracker
// publishes the final "done" and resets. The default is 6 seconds.
// Intended for tests; production code should not call this.
func (ix *Indexer) SetIngestIdleTimeout(d time.Duration) {
	if ix.ingest != nil {
		ix.ingest.idleAfter = d
	}
}

// Enqueue adds path to the processing queue with no batch association.
// Duplicate in-flight paths are silently dropped (only one copy processed at a time).
func (ix *Indexer) Enqueue(path string) {
	// LoadOrStore: if already in flight, skip.
	if _, loaded := ix.seen.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	select {
	case ix.queue <- ingestQueueItem{path: path}:
		ix.ingest.noteEnqueue()
	default:
		// queue full — release the seen lock so it can be retried later
		ix.seen.Delete(path)
	}
}

// EnqueueWithBatch is like Enqueue but associates the file with a named batch.
// batchID identifies a logical group of files (e.g. a single multi-select upload
// session generated by the front end). batchTotal is the total number of files
// expected in the batch; when > 0 the task starts with Progress=0/batchTotal
// rather than growing the total one file at a time.
// If batchID is empty, behaviour is identical to Enqueue.
func (ix *Indexer) EnqueueWithBatch(path, batchID string, batchTotal int64) {
	if batchID == "" {
		ix.Enqueue(path)
		return
	}
	if _, loaded := ix.seen.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	select {
	case ix.queue <- ingestQueueItem{path: path, batchID: batchID}:
		ix.ingest.noteEnqueueWithBatch(batchID, batchTotal)
	default:
		ix.seen.Delete(path)
	}
}

// MarkAndReserve pre-occupies the seen map and records batch metadata for path
// before the file is moved into the gallery directory. This two-step API
// (MarkAndReserve → rename → SubmitReserved) prevents a fsnotify race where
// the watcher fires a Create event for the renamed file and calls Enqueue
// before the TUS goroutine reaches EnqueueWithBatch — causing the file to land
// in the default idle slot (batches[""]) instead of the named batch.
//
// Returns false when path is already occupied in seen (should not normally
// happen); the caller must not proceed with the rename in that case.
func (ix *Indexer) MarkAndReserve(path, batchID string, batchTotal int64) bool {
	if _, loaded := ix.seen.LoadOrStore(path, struct{}{}); loaded {
		return false
	}
	ix.ingest.noteEnqueueWithBatch(batchID, batchTotal)
	return true
}

// SubmitReserved pushes a previously MarkAndReserve-d path into the worker
// queue. If the queue is full the seen reservation is released (batch counter
// already incremented; the idle timer will eventually fire on the remaining
// files and not deadlock).
func (ix *Indexer) SubmitReserved(path, batchID string) {
	select {
	case ix.queue <- ingestQueueItem{path: path, batchID: batchID}:
	default:
		ix.seen.Delete(path)
	}
}

// SetOnBatchDone registers a callback that is called each time any batch
// (including the anonymous "" batch) transitions to "done". Intended for
// upper-layer hooks such as triggering RunClustering after an upload batch
// completes. Safe to call before or after Start.
func (ix *Indexer) SetOnBatchDone(fn func()) {
	if ix.ingest != nil {
		ix.ingest.onBatchDone = fn
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
				case item, ok := <-ix.queue:
					if !ok {
						return
					}
					success := ix.processFile(item.path)
					ix.ingest.noteResultWithBatch(item.batchID, success)
					ix.seen.Delete(item.path)
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

// processOpts controls which stages processFileInternal executes.
type processOpts struct {
	force     bool // skip checksum + status='indexed' short-circuit
	skipExif  bool // skip asset_exif write
	skipThumb bool // skip thumbnail generation
}

// processFile is the entry point from Enqueue → worker pool, with zero options.
// Returns success: true means status='indexed' was written; false means any stage failed.
func (ix *Indexer) processFile(path string) bool {
	return ix.processFileInternal(path, processOpts{})
}

// ForceReprocess is used by the Embedder retry path to bypass the checksum
// short-circuit and optionally skip EXIF / thumbnail stages.
func (ix *Indexer) ForceReprocess(path string, opts processOpts) bool {
	return ix.processFileInternal(path, opts)
}

// processFileInternal runs the full indexing pipeline for a single file.
func (ix *Indexer) processFileInternal(path string, opts processOpts) (success bool) {
	// 1. Read file content.
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	// 2. Compute SHA-256 checksum.
	checksum := sha256File(data)

	// 3. Skip if checksum already exists in DB with status='indexed'.
	// Records with status='pending' (e.g. left by a crash) are intentionally
	// re-processed so they can reach 'indexed' status.
	// When opts.force is set, bypass this short-circuit entirely.
	if !opts.force {
		var existingID string
		err = ix.db.QueryRow(`SELECT id FROM assets WHERE checksum=? AND status='indexed'`, checksum).Scan(&existingID)
		if err == nil {
			// already fully indexed — nothing to do
			return true
		}
	}

	// 4. Detect MIME type and decide image vs. video.
	mime := http.DetectContentType(data)
	ext := strings.ToLower(filepath.Ext(path))
	isVideo := strings.HasPrefix(mime, "video/") || videoExts[ext]

	// 5. Gather metadata.
	var takenAt time.Time
	var durationMs int64
	var exifResult *exif.Result
	var mediaInfo *ffmpeg.MediaInfo
	var keyframePath string
	var keyframeTmpDir string

	if isVideo {
		keyframeTmpDir, err = os.MkdirTemp("", "nimoos-kf-*")
		if err == nil {
			keyframePath, err = ffmpeg.ExtractKeyframe(path, keyframeTmpDir)
			if err != nil {
				keyframePath = ""
			}
		}
		// One ffprobe call gives us everything: duration, dimensions, codecs,
		// frame rate, bit rate, rotation, creation_time, GPS.
		if mi, perr := ffmpeg.Probe(path); perr == nil {
			mediaInfo = mi
			durationMs = mi.DurationMs
			if !mi.TakenAt.IsZero() {
				takenAt = mi.TakenAt
			}
		} else {
			fmt.Fprintf(os.Stderr, "[indexer] ffmpeg.Probe failed for %s: %v — falling back to duration-only probe\n", path, perr)
			durationMs, _ = ffmpeg.GetDurationMs(path)
		}
	} else {
		f, openErr := os.Open(path)
		if openErr == nil {
			exifResult = exif.Parse(f)
			f.Close()
			if exifResult != nil && !exifResult.TakenAt.IsZero() {
				takenAt = exifResult.TakenAt
			}
		}
		// Most JPEGs put dimensions in the SOF marker rather than EXIF.
		// Fall back to image.DecodeConfig (header-only decode) when EXIF lacks them.
		if exifResult != nil && (exifResult.Width == 0 || exifResult.Height == 0) {
			if cfg, _, derr := image.DecodeConfig(bytes.NewReader(data)); derr == nil {
				exifResult.Width = cfg.Width
				exifResult.Height = cfg.Height
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
	// Fall back to file mtime when no embedded capture time was found.
	// Without this, files lacking EXIF DateTime or video creation_time would
	// all collapse to indexed_at and bunch together on the timeline.
	if takenAt.IsZero() && fi != nil {
		takenAt = fi.ModTime()
	}
	originalName := filepath.Base(path)

	_, err = ix.db.Exec(`
		INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
		                   taken_at, duration_ms, is_live_photo_video, status, checksum)
		VALUES(?,?,?,?,?,?,?,0,'pending',?)
		ON CONFLICT(file_path) DO UPDATE SET
		  checksum      = excluded.checksum,
		  file_size     = excluded.file_size,
		  mime_type     = excluded.mime_type,
		  original_name = excluded.original_name,
		  taken_at      = excluded.taken_at,
		  duration_ms   = excluded.duration_ms,
		  status        = 'pending'`,
		assetID, path, fileSize, mime, originalName,
		nullTime(takenAt), sqlNullInt64(durationMs),
		checksum,
	)
	if err != nil {
		return false
	}
	// After upsert, look up the actual asset ID in case it was an existing record.
	if scanErr := ix.db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID); scanErr != nil {
		fmt.Fprintf(os.Stderr, "[indexer] assetID lookup failed for %s: %v\n", path, scanErr)
		return false
	}

	// 7. INSERT/UPDATE asset_exif — images and videos both write their metadata.
	// Skipped when opts.skipExif is set (e.g. Embedder retry path).
	if !opts.skipExif {
		if isVideo && mediaInfo != nil {
			var lat, lon sql.NullFloat64
			if mediaInfo.HasLocation {
				lat = sql.NullFloat64{Float64: mediaInfo.Latitude, Valid: true}
				lon = sql.NullFloat64{Float64: mediaInfo.Longitude, Valid: true}
			}
			if _, err := ix.db.Exec(`
				INSERT INTO asset_exif(asset_id, width, height, latitude, longitude,
				                       video_codec, audio_codec, frame_rate, bit_rate, rotation)
				VALUES(?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(asset_id) DO UPDATE SET
				  width        = excluded.width,
				  height       = excluded.height,
				  latitude     = excluded.latitude,
				  longitude    = excluded.longitude,
				  video_codec  = excluded.video_codec,
				  audio_codec  = excluded.audio_codec,
				  frame_rate   = excluded.frame_rate,
				  bit_rate     = excluded.bit_rate,
				  rotation     = excluded.rotation`,
				assetID,
				mediaInfo.Width, mediaInfo.Height,
				lat, lon,
				mediaInfo.VideoCodec, mediaInfo.AudioCodec,
				mediaInfo.FrameRate, mediaInfo.BitRate, mediaInfo.Rotation,
			); err != nil {
				fmt.Fprintf(os.Stderr, "[indexer] asset_exif video upsert %s: %v\n", assetID, err)
			}
		} else if !isVideo && exifResult != nil {
			var lat, lon sql.NullFloat64
			if exifResult.Latitude != 0 || exifResult.Longitude != 0 {
				lat = sql.NullFloat64{Float64: exifResult.Latitude, Valid: true}
				lon = sql.NullFloat64{Float64: exifResult.Longitude, Valid: true}
			}
			if _, err := ix.db.Exec(`
				INSERT INTO asset_exif(asset_id, width, height, latitude, longitude, make, model,
				                       iso, shutter_speed, aperture, focal_length, orientation)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(asset_id) DO UPDATE SET
				  width         = excluded.width,
				  height        = excluded.height,
				  latitude      = excluded.latitude,
				  longitude     = excluded.longitude,
				  make          = excluded.make,
				  model         = excluded.model,
				  iso           = excluded.iso,
				  shutter_speed = excluded.shutter_speed,
				  aperture      = excluded.aperture,
				  focal_length  = excluded.focal_length,
				  orientation   = excluded.orientation`,
				assetID,
				exifResult.Width, exifResult.Height,
				lat, lon,
				exifResult.Make, exifResult.Model,
				exifResult.ISO, exifResult.ShutterSpeed,
				exifResult.Aperture, exifResult.FocalLength,
				exifResult.Orientation,
			); err != nil {
				fmt.Fprintf(os.Stderr, "[indexer] asset_exif image upsert %s: %v\n", assetID, err)
			}
		}
	}

	// 8. Generate thumbnails.
	// Skipped when opts.skipThumb is set (e.g. Embedder retry path).
	imagePath := path
	if isVideo && keyframePath != "" {
		imagePath = keyframePath
	}
	if keyframeTmpDir != "" {
		defer os.RemoveAll(keyframeTmpDir)
	}

	if !opts.skipThumb && imagePath != "" {
		thumb.Generate(imagePath, assetID, ix.thumbDir) //nolint:errcheck
	}

	// 9. ML inference (only when ML service is ready).
	if ix.ml.IsReady() {
		// Face detection needs full-resolution detail, so it uses the original
		// image (photos) or the full keyframe (videos).
		var faceData []byte
		if isVideo && keyframePath != "" {
			faceData, _ = os.ReadFile(keyframePath)
		} else {
			faceData = data
		}

		// CLIP embedding instead uses the displayed (small) thumbnail — see
		// embedClip — so the stored vector represents the frame the user actually
		// sees and photos/videos share one resize pipeline. Embedding the full-
		// resolution source biased rankings: high-detail video keyframes could
		// outrank better photo matches.
		_ = ix.embedClip(assetID, faceData)

		if len(faceData) > 0 {
			// Face detection + recognition（FacesEnabled 关闭时跳过）。
			if config.Cfg == nil || config.Cfg.FacesEnabled {
				if faces, faceErr := ix.ml.DetectAndRecognizeFaces(faceData); faceErr == nil {
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
							fmt.Fprintf(os.Stderr, "[indexer] failed to insert face_detection %s: %v\n", assetID, err)
						}
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
		fmt.Fprintf(os.Stderr, "[indexer] failed to mark asset indexed %s: %v\n", assetID, err)
		return false
	}
	return true
}

// embedClip computes and stores the CLIP vector for assetID from its displayed
// (small) thumbnail, falling back to the provided full-resolution bytes when the
// thumbnail is unavailable. Centralised so live indexing and the re-embed
// backfill produce identical vectors.
func (ix *Indexer) embedClip(assetID string, fallback []byte) error {
	img := fallback
	if b, err := os.ReadFile(filepath.Join(ix.thumbDir, assetID, "small.jpg")); err == nil && len(b) > 0 {
		img = b
	}
	if len(img) == 0 {
		return fmt.Errorf("embedClip: no image for %s", assetID)
	}
	vec, err := ix.ml.CLIPImageEmbed(img)
	if err != nil {
		return err
	}
	return ix.writeClipEmbedding(assetID, vec)
}

// ReembedAllClip recomputes the CLIP embedding for every indexed asset from its
// displayed thumbnail. One-time backfill after changing the embedding input so
// existing assets match the new (thumbnail-based) pipeline. Returns counts of
// (succeeded, failed). Faces/EXIF/thumbnails are left untouched.
func (ix *Indexer) ReembedAllClip() (succeeded, failed int) {
	rows, err := ix.db.Query(`SELECT id FROM assets WHERE status='indexed' AND deleted_at IS NULL`)
	if err != nil {
		return 0, 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := ix.embedClip(id, nil); err != nil {
			failed++
		} else {
			succeeded++
		}
	}
	return succeeded, failed
}

// writeClipEmbedding upserts the CLIP embedding for the given asset.
// clip_embeddings is a sqlite-vec vec0 virtual table that does NOT support
// INSERT OR REPLACE, so we try UPDATE first and fall back to INSERT when no row
// is affected. That also self-heals partial state (asset_clip_idx row exists
// but clip_embeddings row was never written).
func (ix *Indexer) writeClipEmbedding(assetID string, vec []float32) error {
	var rowid int64
	err := ix.db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid)
	if err != nil {
		res, err2 := ix.db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
		if err2 == nil {
			rowid, _ = res.LastInsertId()
		}
	}
	if rowid <= 0 {
		return fmt.Errorf("writeClipEmbedding: no rowid for %s", assetID)
	}
	blob := sqlite.SerializeFloat32(vec)
	res, err := ix.db.Exec(`UPDATE clip_embeddings SET embedding=? WHERE rowid=?`, blob, rowid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] update clip_embeddings %s: %v\n", assetID, err)
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := ix.db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, blob); err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] insert clip_embeddings %s: %v\n", assetID, err)
		return err
	}
	return nil
}

// sha256File returns the hex-encoded SHA-256 hash of data.
func sha256File(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// walkSupported recursively walks dir, calling fn for each supported media file.
// Directories whose names begin with "." (e.g. .trash) are skipped entirely so
// that soft-deleted files are never re-indexed.
func walkSupported(dir string, fn func(path string)) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if supportedExts[strings.ToLower(filepath.Ext(path))] {
			fn(path)
		}
		return nil
	})
}

// ScanDirectory walks dir, then synchronously processes each file. This is
// intentionally serial (vs. the Enqueue/worker-pool path used by watcher) so
// that we can report deterministic per-scan progress via TaskRegistry —
// async workers can't easily associate per-file completion with a single
// scan task ID. Throughput trade-off is acceptable because scans run in a
// background goroutine and are not user-blocking.
//
// ScanDirectory walks dir, processes all supported media files found on disk,
// and prunes asset rows under dir whose backing files no longer exist.
// If a TaskRegistry has been injected via SetTaskRegistry, scan progress is
// reported as a running "index" task and cleared 6 seconds after completion.
func (ix *Indexer) ScanDirectory(dir string) error {
	// First pass: collect all supported file paths to know the total.
	var paths []string
	if err := walkSupported(dir, func(p string) { paths = append(paths, p) }); err != nil {
		return err
	}

	total := int64(len(paths))
	scanStart := time.Now()
	taskID := fmt.Sprintf("idx_%d", scanStart.UnixNano())
	var processed int64

	if ix.taskReg != nil && total > 0 {
		ix.taskReg.Upsert(Task{
			ID:        taskID,
			Type:      "index",
			Label:     "索引照片",
			Current:   0,
			Total:     total,
			Progress:  0,
			Status:    "running",
			StartedAt: scanStart,
		})
	}

	defer func() {
		if ix.taskReg == nil || total == 0 {
			return
		}
		ix.taskReg.Upsert(Task{
			ID:        taskID,
			Type:      "index",
			Label:     "索引照片",
			Current:   processed,
			Total:     total,
			Progress:  1,
			Status:    "done",
			StartedAt: scanStart,
		})
		go func() {
			time.Sleep(taskCleanupDelay)
			ix.taskReg.Remove(taskID)
		}()
	}()

	// Second pass: process each file and report progress.
	for _, path := range paths {
		_ = ix.processFile(path)
		// Mirror the async worker path (Start): clear the in-flight marker so
		// that watcher-triggered Enqueue calls for the same file after the scan
		// are not silently dropped by seen.LoadOrStore.
		ix.seen.Delete(path)
		processed++
		if reg := ix.taskReg; reg != nil {
			progress := 0.0
			if total > 0 {
				progress = float64(processed) / float64(total)
			} else {
				progress = float64(processed) * 0.005
				if progress > 0.99 {
					progress = 0.99
				}
			}
			reg.Upsert(Task{
				ID:        taskID,
				Type:      "index",
				Label:     "索引照片",
				Current:   processed,
				Total:     total,
				Progress:  progress,
				Status:    "running",
				StartedAt: scanStart,
			})
		}
	}

	return ix.pruneMissingUnder(dir)
}

// RemoveByPath deletes the asset row for path (if any) and removes its
// thumbnail directory from disk. Safe to call for paths that are not indexed.
func (ix *Indexer) RemoveByPath(path string) {
	var id string
	// Never remove a soft-deleted (trashed) asset: its file legitimately lives
	// under .trash and the watcher must not treat the move as a real deletion.
	err := ix.db.QueryRow(`SELECT id FROM assets WHERE file_path = ? AND deleted_at IS NULL`, path).Scan(&id)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		return
	}
	if _, err := ix.db.Exec(`DELETE FROM assets WHERE id = ?`, id); err != nil {
		return
	}
	if ix.thumbDir != "" && id != "" {
		_ = os.RemoveAll(filepath.Join(ix.thumbDir, id))
	}
	ix.seen.Delete(path)
}

// pruneMissingUnder removes asset rows whose file_path is under dir but whose
// file no longer exists on disk. Thumbnails for removed assets are deleted too.
func (ix *Indexer) pruneMissingUnder(dir string) error {
	prefix := strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator)
	rows, err := ix.db.Query(
		`SELECT id, file_path FROM assets WHERE file_path = ? OR file_path LIKE ?`,
		dir, prefix+"%",
	)
	if err != nil {
		return fmt.Errorf("pruneMissingUnder query: %w", err)
	}
	type row struct{ id, path string }
	var gone []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return err
		}
		if _, statErr := os.Stat(r.path); os.IsNotExist(statErr) {
			gone = append(gone, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range gone {
		if _, err := ix.db.Exec(`DELETE FROM assets WHERE id = ?`, r.id); err != nil {
			continue
		}
		if ix.thumbDir != "" {
			_ = os.RemoveAll(filepath.Join(ix.thumbDir, r.id))
		}
		ix.seen.Delete(r.path)
	}
	return nil
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

// MLReady reports whether the ML backend (immich-machine-learning) is reachable.
// Bounded by the ml client's short /ping timeout, safe to call from handlers.
func (ix *Indexer) MLReady() bool { return ix.ml.IsReady() }

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

	// Total bytes of all indexed assets — used by the UI to display real
	// gallery footprint independent of filesystem-level disk usage.
	_ = ix.db.QueryRow(
		`SELECT COALESCE(SUM(file_size), 0) FROM assets WHERE status = 'indexed'`,
	).Scan(&s.TotalBytes)
	return s
}

// sqlNullInt64 converts int64 to sql.NullInt64 (zero → invalid).
func sqlNullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: v}
}
