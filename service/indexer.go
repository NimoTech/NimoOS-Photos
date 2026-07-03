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
	"sync/atomic"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
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
	OCR(imageData []byte) ([]mlclient.OCRLine, error)
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
	".gif":  true,
	".bmp":  true,
	".tiff": true,
	".tif":  true,
	".avif": true,
	".mp4":  true,
	".mov":  true,
	".mkv":  true,
	".avi":  true,
	".webm": true,
	".m4v":  true,
	".3gp":  true,
}

// scanExcludeDirs are absolute directory paths excluded from scanning even
// though their names don't start with ".". They hold app/system data, not
// user media. (.system_data is already skipped by the dot-prefix rule.)
var scanExcludeDirs = map[string]bool{
	"/DATA/AppData":    true,
	"/DATA/lost+found": true,
}

// videoExts are extensions treated as video regardless of MIME detection.
var videoExts = map[string]bool{
	".mov":  true,
	".mp4":  true,
	".mkv":  true,
	".avi":  true,
	".webm": true,
	".m4v":  true,
	".3gp":  true,
}

// canonicalMime maps the media extensions we index to their authoritative MIME
// type. http.DetectContentType cannot recognise some of the containers we
// support — it returns application/octet-stream for QuickTime (.mov) and HEIC,
// and the misleading video/webm for Matroska (.mkv). The whole system keys off
// the stored mime_type (the frontend selects <video> vs <img> from it, and every
// "mime_type LIKE 'video/%'" query depends on it), so we persist the canonical
// type derived from the extension instead of trusting the content sniff.
var canonicalMime = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".heic": "image/heic",
	".webp": "image/webp",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".tiff": "image/tiff",
	".tif":  "image/tiff",
	".avif": "image/avif",
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
	".webm": "video/webm",
	".m4v":  "video/mp4",
	".3gp":  "video/3gpp",
}

// resolveMimeType returns the canonical MIME type for a supported media
// extension, falling back to content sniffing for anything not in the table.
func resolveMimeType(data []byte, ext string) string {
	if m, ok := canonicalMime[strings.ToLower(ext)]; ok {
		return m
	}
	return http.DetectContentType(data)
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
	db         *sql.DB
	ml         MLProvider
	thumbDir   string
	workers    int
	queue      chan ingestQueueItem
	seen       sync.Map // in-flight dedup: path -> struct{}
	taskReg    *TaskRegistry
	ingest     *ingestTracker // aggregates Enqueue/processFile into a single rolling task
	scanActive int32          // CAS guard so only one full ScanAllRoots runs at a time

	// mountRoots returns the currently-mounted scan roots. pruneMissingUnder
	// consults it as a safety interlock: it refuses to prune a directory whose
	// backing mount has vanished (e.g. a USB drive unplugged right after its
	// post-remount rescan started, leaving an empty leftover mount dir — every
	// file would stat as missing and the whole library on that drive would be
	// wiped, vectors and thumbnails included). Defaults to EnumerateScanRoots;
	// injectable so tests don't depend on the real /proc/mounts.
	mountRoots func() []string

	// lastActivity 记录最近一次入队/处理完成的时刻(UnixNano)。人脸聚类的「安全网」
	// 触发(scheduler 每分钟那条)据此去抖:仅在索引活动安静一段时间后才触发,
	// 避免大上传途中索引队列出现 pending==0 空档时被误判为「上传结束」而提前聚类。
	lastActivity atomic.Int64

	// pendingAlbum maps a gallery path to the album the upload requested.
	// Registered before SubmitReserved (no race with workers) and taken
	// (removed) when the worker starts processing the path, so failed files
	// don't leak entries.
	pendingAlbumMu sync.Mutex
	pendingAlbum   map[string]string

	// albumAssigner is called after an asset record is successfully written,
	// with the asset's DB id and the album id from pendingAlbum.
	albumAssigner func(assetID, albumID string)
}

// touch marks index activity (enqueue or a processed result) at the current time.
func (ix *Indexer) touch() { ix.lastActivity.Store(time.Now().UnixNano()) }

// IdleFor reports how long since the last enqueue/processed result. Returns a
// very large duration if there has been no activity yet (treated as "idle").
func (ix *Indexer) IdleFor() time.Duration {
	last := ix.lastActivity.Load()
	if last == 0 {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(time.Unix(0, last))
}

// ScanAllRoots scans every user-accessible partition returned by
// EnumerateScanRoots (the system disk plus mounted RAID/USB drives). A CAS
// guard ensures only one full scan runs at a time: concurrent triggers
// (startup, periodic ticker, manual rescan) arriving while a scan is already
// in progress return immediately instead of spawning a duplicate scan — which
// is what previously surfaced as two identical "索引照片" tasks at once.
// Callers run this in their own goroutine; it blocks until all roots are done.
func (ix *Indexer) ScanAllRoots() {
	if !atomic.CompareAndSwapInt32(&ix.scanActive, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&ix.scanActive, 0)
	for _, dir := range EnumerateScanRoots() {
		_ = ix.ScanDirectory(dir)
	}
}

// pruneSystemMountAssets removes any indexed asset that lives under a known
// system mount (see systemMounts). An earlier over-broad scan indexed OS files
// (e.g. the /media/root-ro Adwaita icon themes); this runs at startup so that
// pollution self-heals and can never linger once the scan scope is corrected.
// It reuses RemoveByPath so the asset row, its cascaded rows and its thumbnails
// are all cleaned the same way a normal deletion would.
func (ix *Indexer) pruneSystemMountAssets() {
	for mp := range systemMounts {
		rows, err := ix.db.Query(`SELECT file_path FROM assets WHERE file_path = ? OR file_path LIKE ?`, mp, mp+"/%")
		if err != nil {
			continue
		}
		var paths []string
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				paths = append(paths, p)
			}
		}
		rows.Close()
		for _, p := range paths {
			ix.RemoveByPath(p)
		}
	}
}

// defaultIngestIdleTimeout is the quiet period after which ingestTracker
// publishes the final "done" task and resets itself for the next batch.
const defaultIngestIdleTimeout = 6 * time.Second

// taskCleanupDelay is how long a completed task stays in the registry before
// being removed. Shared by ingestTracker, ScanDirectory, and FaceService so
// the UI has a consistent window to display the done state.
const taskCleanupDelay = 6 * time.Second

// tusEchoSuppress is how long a TUS-ingested file's seen reservation is held
// after the worker finishes, to swallow the watcher's late fsnotify Create echo
// of the just-renamed file (which would otherwise spawn a stray batches[""]
// "索引照片" task). Generous enough to cover inotify delivery backlog under load.
const tusEchoSuppress = 30 * time.Second

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
		db:           db,
		ml:           ml,
		thumbDir:     thumbDir,
		workers:      workers,
		queue:        make(chan ingestQueueItem, 1024),
		ingest:       newIngestTracker(),
		pendingAlbum: make(map[string]string),
		mountRoots:   EnumerateScanRoots,
	}
}

// SetPendingAlbum registers that the file at path should be added to albumID
// after it is indexed. A no-op when albumID is empty. Must be called before
// SubmitReserved so the worker cannot pick up the item before the entry is set.
func (ix *Indexer) SetPendingAlbum(path, albumID string) {
	if albumID == "" {
		return
	}
	ix.pendingAlbumMu.Lock()
	ix.pendingAlbum[path] = albumID
	ix.pendingAlbumMu.Unlock()
}

// takePendingAlbum reads and removes the pending album entry for path.
// Returns "" when no entry is present.
func (ix *Indexer) takePendingAlbum(path string) string {
	ix.pendingAlbumMu.Lock()
	albumID := ix.pendingAlbum[path]
	delete(ix.pendingAlbum, path)
	ix.pendingAlbumMu.Unlock()
	return albumID
}

// SetAlbumAssigner registers a callback that is invoked after each asset record
// is successfully written, when a pending album was registered for the file.
// Call this after construction (e.g. from NewService) before any scans begin.
// Same injection pattern as SetOnBatchDone and SetTaskRegistry.
func (ix *Indexer) SetAlbumAssigner(fn func(assetID, albumID string)) {
	ix.albumAssigner = fn
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
		ix.touch()
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
		ix.touch()
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
	ix.touch()
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
					ix.touch()
					if item.batchID != "" {
						// TUS 上传的文件:rename 落地会让 watcher 迟发一个 Create 事件。
						// 大批量 + inotify 压力下,该事件常常晚于 worker 处理完成才到达,
						// 此时若立即释放 seen,watcher 的 Enqueue 会把这个「已索引」文件
						// 当成 legacy "" 累积批次重新入队 → 冒出第二个「索引照片」任务(dedup
						// 秒完→闪 100%),并连带在下一次 settle 触发又一次聚类。
						// 延迟释放 seen 以吸收这个回声;普通 watcher 新文件(batchID="")
						// 维持立即释放,不影响正常落盘检测。
						p := item.path
						time.AfterFunc(tusEchoSuppress, func() { ix.seen.Delete(p) })
					} else {
						ix.seen.Delete(item.path)
					}
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
	// Consume pending album entry immediately so that a failed file does not
	// leave a stale entry in the map.
	pendingAlbumID := ix.takePendingAlbum(path)

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
			// already fully indexed — assign to album if requested, then short-circuit
			if pendingAlbumID != "" && ix.albumAssigner != nil {
				ix.albumAssigner(existingID, pendingAlbumID)
			}
			return true
		}
	}

	// 4. Detect MIME type and decide image vs. video.
	ext := strings.ToLower(filepath.Ext(path))
	mime := resolveMimeType(data, ext)
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

	// Wire pending album assignment: if the upload requested album membership,
	// add the asset to the album now that its DB record exists.
	// albumAssigner is nil-guarded; AddAsset uses INSERT OR IGNORE (idempotent).
	if pendingAlbumID != "" && ix.albumAssigner != nil {
		ix.albumAssigner(assetID, pendingAlbumID)
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
		// CLIP embedding（ScenesEnabled 关闭时跳过——注意嵌入同时是语义搜索的基础，
		// 关闭后新照片不参与语义搜索）。
		if config.Cfg == nil || config.Cfg.ScenesEnabled {
			_ = ix.embedClip(assetID, faceData)
		}

		if len(faceData) > 0 {
			// Face detection + recognition（FacesEnabled 关闭时跳过）。
			if config.Cfg == nil || config.Cfg.FacesEnabled {
				if faces, faceErr := ix.ml.DetectAndRecognizeFaces(faceData); faceErr == nil {
					for _, face := range faces {
						if len(face.Embedding) != common.FaceDim {
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

			// OCR uses the same full-detail input as faces (original photo or
			// full keyframe) — small text on receipts/documents is lost at
			// thumbnail resolution.
			// 视频不跑 OCR:对视频关键帧做 OCR 没有实际意义,还会把「录屏/含文字画面」的
			// 视频误判进「OCR/文档」分类(asset_ocr 命中即归类)。视频只保留 CLIP 用于
			// 视觉检索;真正的视频理解(分段 embedding)是后续工作。
			// OCR（OCREnabled 关闭或视频时跳过）。
			if !isVideo && (config.Cfg == nil || config.Cfg.OCREnabled) {
				if err := ix.ocrAsset(assetID, faceData); err != nil {
					fmt.Fprintf(os.Stderr, "[indexer] OCR failed for %s: %v\n", assetID, err)
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

// minOCRScore is the recognition-confidence floor below which OCR lines are
// discarded as noise before being stored.
const minOCRScore = 0.5

// quadArea returns the area of a quadrilateral given as 8 normalized floats
// (x1,y1,…,x4,y4) via the shoelace formula. Result is in [0,1] image-area
// units since the coordinates are normalized.
func quadArea(c []float64) float64 {
	if len(c) != 8 {
		return 0
	}
	s := 0.0
	for i := 0; i < 4; i++ {
		x1, y1 := c[i*2], c[i*2+1]
		x2, y2 := c[((i+1)%4)*2], c[((i+1)%4)*2+1]
		s += x1*y2 - x2*y1
	}
	if s < 0 {
		s = -s
	}
	return s / 2
}

// ocrAsset runs OCR on imageData and upserts the recognized text into
// asset_ocr, together with the document-ness signals used by the OCR media
// category: line_count (kept lines) and coverage (total text-box area as a
// fraction of the image). A row is written even when no text is found
// (text=”), so the backfill can tell "OCR done, empty" apart from "not yet
// OCR'd".
func (ix *Indexer) ocrAsset(assetID string, imageData []byte) error {
	lines, err := ix.ml.OCR(imageData)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(lines))
	coverage := 0.0
	for _, l := range lines {
		if l.Score >= minOCRScore && strings.TrimSpace(l.Text) != "" {
			kept = append(kept, l.Text)
			coverage += quadArea(l.Box)
		}
	}
	if coverage > 1 {
		coverage = 1
	}
	_, err = ix.db.Exec(`
		INSERT INTO asset_ocr(asset_id, text, coverage, line_count, ocr_at)
		VALUES(?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(asset_id) DO UPDATE SET
		  text=excluded.text, coverage=excluded.coverage,
		  line_count=excluded.line_count, ocr_at=excluded.ocr_at`,
		assetID, strings.Join(kept, "\n"), coverage, len(kept))
	return err
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
			if scanExcludeDirs[path] {
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
// dropClipVector removes an asset's CLIP embedding from the sqlite-vec vec0
// table. It MUST run before the asset row is deleted: asset_clip_idx (the
// asset_id->rowid map) is removed by FK cascade when the asset goes, and the
// vec0 row cannot be reached afterwards (a foreign-key cascade cannot follow
// into a virtual table). Every permanent-delete path (RemoveByPath, scan prune,
// trash purge, direct DeleteAsset) calls this so deleted assets never leave
// orphan vectors that waste KNN top-k slots and degrade CLIP search. Free
// function (takes *sql.DB) so non-Indexer services can reuse it.
func dropClipVector(db *sql.DB, assetID string) {
	var rowid int64
	if db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid) == nil {
		_, _ = db.Exec(`DELETE FROM clip_embeddings WHERE rowid=?`, rowid)
	}
}

// pruneOrphanClipVectors sweeps orphaned CLIP vectors in two passes — a cheap
// (pure-SQL, no ML) safety net run at startup and daily. It is NOT a reindex;
// it never re-embeds, it only deletes dangling rows.
//
// Pass 1: asset_clip_idx rows whose asset no longer exists (seen in production:
// historical deletes that bypassed the FK cascade left idx rows behind, and
// their vectors kept occupying KNN top-k slots). Delete those vectors first
// (the idx row is the only way to reach them), then the idx rows.
// Pass 2: vec0 rows with no idx mapping at all.
func pruneOrphanClipVectors(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM clip_embeddings WHERE rowid IN
		(SELECT rowid FROM asset_clip_idx WHERE asset_id NOT IN (SELECT id FROM assets))`)
	_, _ = db.Exec(`DELETE FROM asset_clip_idx WHERE asset_id NOT IN (SELECT id FROM assets)`)
	_, _ = db.Exec(`DELETE FROM clip_embeddings WHERE rowid NOT IN (SELECT rowid FROM asset_clip_idx)`)
}

// pruneVideoOCR 删除视频的 OCR 行。视频不再跑 OCR(关键帧 OCR 无意义,还会把含文字
// 画面的视频误判进「OCR/文档」分类);这里在启动时清掉历史遗留的视频 OCR 行,使已索引
// 的视频也立即退出 OCR 分类。幂等:之后视频不再产生 asset_ocr 行。
func pruneVideoOCR(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM asset_ocr WHERE asset_id IN
		(SELECT id FROM assets WHERE mime_type LIKE 'video/%')`)
}

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
	dropClipVector(ix.db, id) // before the cascade drops asset_clip_idx
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
//
// Two interlocks protect against wiping a removable drive's library:
//  1. dir must still sit under a currently-mounted scan root (ix.mountRoots).
//     If the mount vanished between the scan starting and the prune (unplug
//     right after replug, leaving an empty leftover mount dir), every file
//     stats as missing and pruning would physically delete every asset row,
//     CLIP vector and thumbnail for that drive.
//  2. offline=1 assets are excluded: their files being unreachable is exactly
//     the state the flag records, not evidence of deletion.
func (ix *Indexer) pruneMissingUnder(dir string) error {
	if !ix.dirUnderMountedRoot(dir) {
		zap.L().Warn("pruneMissingUnder: directory not under any mounted scan root, skipping prune",
			zap.String("dir", dir))
		return nil
	}
	// substr() prefix compare instead of LIKE: mount/directory names routinely
	// contain LIKE metacharacters (`_` in USB labels like Kingston_DataTra
	// matches any character and would bleed onto sibling directories).
	prefix := strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator)
	rows, err := ix.db.Query(
		`SELECT id, file_path FROM assets
		 WHERE offline = 0 AND (file_path = ? OR substr(file_path,1,length(?)) = ?)`,
		dir, prefix, prefix,
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
		dropClipVector(ix.db, r.id) // before the cascade drops asset_clip_idx
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

// dirUnderMountedRoot reports whether dir is one of (or lives under one of)
// the currently-mounted scan roots. Roots always include /DATA, so library
// directories on the system disk are always eligible; a /media/* mount that
// has vanished from the mount table is not.
func (ix *Indexer) dirUnderMountedRoot(dir string) bool {
	mounts := ix.mountRoots
	if mounts == nil {
		mounts = EnumerateScanRoots
	}
	cleaned := strings.TrimRight(dir, string(filepath.Separator))
	for _, root := range mounts() {
		r := strings.TrimRight(root, string(filepath.Separator))
		if cleaned == r || strings.HasPrefix(cleaned, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
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

	// Offline count is independent of the status breakdown above: offline=1
	// assets keep whatever status they had (usually 'indexed') and are still
	// counted there — this is a separate, additive figure surfaced to the UI.
	_ = ix.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE offline = 1`).Scan(&s.Offline)
	return s
}

// boolToInt maps a bool to the SQLite integer-boolean convention (0/1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sqlNullInt64 converts int64 to sql.NullInt64 (zero → invalid).
func sqlNullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: v}
}
