package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/aesthetic"
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

// mimeSniffBytes is the max number of header bytes read when an unknown
// extension falls back to content sniffing. http.DetectContentType itself
// only looks at the first 512B; this leaves a bit of headroom. Known
// extensions (canonicalMime hits) are unaffected by this constant at all —
// they never read the file.
const mimeSniffBytes = 4096

// readHeader reads at most the first n bytes of path, for cases that only
// need the file's header (e.g. MIME content sniffing), avoiding reading the
// whole file into memory the way os.ReadFile would. The file being smaller
// than n bytes is normal (returns whatever was read, not an error).
func readHeader(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	nr, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:nr], nil
}

// detectMimeType is the actual MIME-detection entry point called from
// processFileInternal: known extensions go straight to the canonicalMime
// table without touching disk at all; only unrecognized extensions read the
// file header (mimeSniffBytes) for content sniffing — replacing the old
// "read the whole file into memory, then sniff" approach (even a multi-GB
// video now reads at most 4KB during MIME detection).
func detectMimeType(path, ext string) string {
	if m, ok := canonicalMime[strings.ToLower(ext)]; ok {
		return m
	}
	header, err := readHeader(path, mimeSniffBytes)
	if err != nil {
		header = nil
	}
	return resolveMimeType(header, ext)
}

// maxImageReadBytes is the byte cap on reading an image's original file
// fully into memory during indexing (used for ML faceData and as a fallback
// for image.DecodeConfig dimensions), to keep an abnormally huge/disguised
// image file from blowing up the Go process's resident memory. Note this is
// a different concern from oversizedForML (mlinput.go, the 178.9MP pixel cap
// on what gets fed to immich-ml /predict) — don't conflate the two: that one
// decides whether an image "can be fed to ML", this one decides whether an
// image "should be read into memory whole".
// Declared as a var rather than const so tests can inject a smaller
// threshold without actually having to create a 100MB+ file in a test.
var maxImageReadBytes int64 = 100 * 1024 * 1024 // 100MB

// imageExceedsReadLimit is a pure predicate: whether the file size exceeds
// maxImageReadBytes. Pulled out so boundary values can be tested without
// constructing a real large file.
func imageExceedsReadLimit(size int64) bool {
	return size > maxImageReadBytes
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

	// scanDirInFlight dedups whole-directory rescans: both the watcher's
	// mount polling (followMounts) and MountGuard's replug recovery can
	// trigger a rescan for the same mount, so only one ScanDirectory is
	// allowed to run per dir at a time, avoiding a redundant full scan
	// wasting IO (see ScanDirectoryOnce).
	scanDirInFlight sync.Map // dir -> struct{}

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

	// lastActivity records the time (UnixNano) of the most recent enqueue or
	// processed result. The face-clustering "safety net" trigger (the
	// scheduler's once-a-minute check) debounces off this: it only fires
	// once index activity has been quiet for a while, so a pending==0 gap
	// mid-upload isn't mistaken for "upload finished" and clustering doesn't
	// fire prematurely.
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

	// In-process cache of doc-classification prompt vectors (see
	// docverdict.go loadPromptVecs).
	promptMu    sync.Mutex
	promptDoc   [][]float32
	promptPhoto [][]float32

	// aestheticHead, when non-nil, makes writeClipEmbedding compute and store
	// the aesthetic score inline after a successful write (pure local matrix
	// multiply, microsecond-scale). Injected via SetAestheticHead; nil when
	// AestheticEnabled=false.
	aestheticHead *aesthetic.Head

	// sprites is the process-wide shared hover-preview sprite generator: the
	// indexing pipeline's inline pregeneration, the startup backfill
	// (BackfillSprites), and the /sprite route's on-demand generation must
	// all share this one instance so its in-flight dedup can prevent
	// concurrent ffmpeg runs writing the same output file.
	sprites *SpriteGenerator

	// spriteBackfillRunning is BackfillSprites' CAS re-entrancy latch: only
	// one backfill pass over existing assets is allowed to run at a time,
	// preventing a service-restart storm or a spurious trigger from scanning
	// the same candidate batch concurrently.
	spriteBackfillRunning atomic.Bool

	// previewPregen mirrors the photos.PreviewPregen config (injected via
	// SetPreviewPregen): false (the default) skips preview.mp4 generation
	// both in the inline indexing path and in BackfillSprites' startup
	// backfill, leaving only the /preview route's lazy generation;
	// sprite.jpg is unaffected.
	previewPregen bool

	// onIndexed is called once, asynchronously, after an asset is
	// successfully written as status='indexed' (the single write point, see
	// the end of processFileInternal), for CaptionFeeder.FeedOne's inline
	// feed hook. Injected as a function field (same pattern as
	// albumAssigner/onBatchDone) so Indexer doesn't depend directly on the
	// CaptionFeeder type; safely skipped when nil (not wired up / tests).
	onIndexed func(assetID string)

	// onCaptionDelete is called after an asset is hard-deleted
	// (RemoveByPath/pruneMissingUnder, right next to the dropClipVector call
	// site) succeeds, for CaptionFeeder.DeleteRemote to hook into (Task 4).
	// Injected as a function field, same as onIndexed; safely skipped when nil.
	onCaptionDelete func(assetID string)
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
// is what previously surfaced as two identical "Indexing photos" tasks at once.
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

// pruneSystemMountAssets removes any indexed asset that lives under a mount
// IsExcludedMount says Photos must never index: a known OS system mount (see
// systemMounts) or a devmon removable-media mount (see excludedMountPrefixes).
// An earlier over-broad scan indexed OS files (e.g. the /media/root-ro Adwaita
// icon themes); the devmon case is the product decision to stop indexing USB
// drives entirely (legacy assets predating that decision, ~338 on the
// production box at the time of writing). This runs at startup so both kinds
// of pollution self-heal and can never linger once the scan scope is
// corrected/narrowed. It reuses RemoveByPath so the asset row, its cascaded
// rows (face_detections via FK, CLIP vector via dropClipVector) and its
// thumbnails are all cleaned the same way a normal deletion would.
func (ix *Indexer) pruneSystemMountAssets() {
	for mp := range systemMounts {
		ix.prunePathsMatching(`file_path = ? OR file_path LIKE ?`, mp, mp+"/%")
	}
	for _, prefix := range excludedMountPrefixes {
		// prefix (e.g. "/media/devmon/") is a fixed literal with no LIKE
		// metacharacters of its own, so a plain suffix-wildcard LIKE is safe
		// here — unlike MountGuard's per-drive-label offline comparisons, the
		// variable part (the USB label) is DATA being matched against the
		// pattern, not woven INTO the pattern, so it cannot misfire onto a
		// sibling label the way `LIKE 'disk_A/%'` would match "diskXA".
		ix.prunePathsMatching(`file_path LIKE ?`, prefix+"%")
	}
}

// pruneRcloneMountAssets removes any indexed asset living under an rclone
// FUSE cloud-drive mount. Cloud drives are excluded from scanning/watching
// (see parseScanRoots) — this startup purge self-heals whatever an earlier,
// broader scan may have indexed. mounts is passed in by the caller as
// enumerateRcloneMounts(); injected as a parameter for testability. An
// unmounted cloud drive is left untouched — we don't guess at its path
// pattern.
// Mount point names contain `_` (rclone names them
// /mnt/<user>_<provider>_<id>), which is a LIKE single-char wildcard, so a
// substr prefix compare must be used instead of LIKE.
func (ix *Indexer) pruneRcloneMountAssets(mounts []string) {
	for _, mp := range mounts {
		prefix := strings.TrimRight(mp, "/") + "/"
		ix.prunePathsMatching(
			`file_path = ? OR substr(file_path,1,length(?)) = ?`,
			mp, prefix, prefix,
		)
	}
}

// prunePathsMatching deletes every asset whose file_path matches the given SQL
// WHERE fragment (against args), via RemoveByPath. Shared helper for
// pruneSystemMountAssets' two exclusion kinds (exact system mounts, devmon
// prefix).
func (ix *Indexer) prunePathsMatching(where string, args ...any) {
	rows, err := ix.db.Query(`SELECT file_path FROM assets WHERE `+where, args...)
	if err != nil {
		return
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
// "Indexing photos" task). Generous enough to cover inotify delivery backlog under load.
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
		Label:     "Indexing photos",
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
	label := "Indexing photos"
	if b.failed > 0 {
		label = fmt.Sprintf("Indexing photos (%d failed)", b.failed)
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
		sprites:      NewSpriteGenerator(),
	}
}

// Sprites returns the process-wide shared sprite generator: the indexing
// pipeline's inline pregeneration, the startup backfill, and the /sprite
// route must all share this one instance so its in-flight dedup can prevent
// concurrent ffmpeg runs writing the same output file.
func (ix *Indexer) Sprites() *SpriteGenerator { return ix.sprites }

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

// SetPreviewPregen injects the photos.PreviewPregen config: false (the
// default) skips preview.mp4 pregeneration in both the indexing pipeline and
// the startup backfill, leaving only the route's lazy generation.
func (ix *Indexer) SetPreviewPregen(on bool) { ix.previewPregen = on }

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
	if isInSnapshotsDir(path) {
		return
	}
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
	if isInSnapshotsDir(path) {
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
// happen) or when path sits under a ".snapshots" directory component
// (isInSnapshotsDir — should never happen either, since callers always
// target the gallery directory, but this keeps MarkAndReserve consistent with
// every other ingestion entry point); the caller must not proceed with the
// rename in that case.
func (ix *Indexer) MarkAndReserve(path, batchID string, batchTotal int64) bool {
	if isInSnapshotsDir(path) {
		return false
	}
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

// SetOnIndexed registers a callback invoked (asynchronously, via `go fn(id)`)
// each time an asset is successfully written as status='indexed'. Intended for
// CaptionFeeder's inline feed hook. Call this after construction, before any
// scans begin. Same injection pattern as SetOnBatchDone/SetAlbumAssigner.
func (ix *Indexer) SetOnIndexed(fn func(assetID string)) {
	ix.onIndexed = fn
}

// SetCaptionDelete injects the caption-delete callback invoked after an asset
// is hard-deleted (typically CaptionFeeder.DeleteRemote), called from
// RemoveByPath/pruneMissingUnder (Task 4).
func (ix *Indexer) SetCaptionDelete(fn func(assetID string)) {
	ix.onCaptionDelete = fn
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
						// For TUS-uploaded files: landing the rename makes the watcher
						// fire a late Create event. Under heavy batches + inotify
						// pressure, that event routinely arrives after the worker has
						// already finished processing — if seen were released
						// immediately, the watcher's Enqueue would re-enqueue this
						// already-indexed file into the legacy "" accumulate batch,
						// spawning a second "Indexing photos" task (dedup
						// completes instantly → flashes to 100%), and triggering another
						// clustering pass on the next settle. Delay releasing seen to
						// absorb this echo; ordinary watcher-discovered new files
						// (batchID="") still release immediately, which doesn't affect
						// normal on-disk detection.
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

// processFileInternal runs the full indexing pipeline for a single file. This
// is the final choke point every ingestion path eventually reaches (worker
// queue, ScanDirectory's synchronous loop, and ForceReprocess), so the
// ".snapshots" guard here is the last line of defense even if some future
// ingestion path forgets to call isInSnapshotsDir directly (see the doc
// comment on isInSnapshotsDir, service/snapshots.go, for the full list of
// entry points this is layered with).
func (ix *Indexer) processFileInternal(path string, opts processOpts) (success bool) {
	if isInSnapshotsDir(path) {
		return false
	}
	// Consume pending album entry immediately so that a failed file does not
	// leave a stale entry in the map.
	pendingAlbumID := ix.takePendingAlbum(path)

	// 1. stat first, without reading a single byte. size+mtime are both the
	// basis for the P2 fast-skip check below and reused directly as the
	// file_size/mtime the INSERT stage needs, avoiding the file being
	// replaced mid-processing causing two stats to disagree.
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileSize := fi.Size()
	mtime := fi.ModTime().UnixNano()

	// 2. stat fast-skip (P2): a file that's already status='indexed' with
	// unchanged file_size+mtime can be confirmed as "already processed"
	// without reading a single byte — this is the key step that breaks the
	// "restart → read every pending row in full → OOM again → killed and
	// restarted again" death loop. Legacy rows (written before this
	// upgrade, mtime column still NULL) necessarily miss here and fall
	// through to the streaming-hash + checksum dedup below; when that hits
	// the checksum short-circuit at step 4, it backfills mtime onto this
	// file_path in place, so the next restart/rescan hits this check
	// directly and skips the read entirely.
	if !opts.force {
		var existingID string
		if err := ix.db.QueryRow(
			`SELECT id FROM assets WHERE file_path=? AND file_size=? AND mtime=? AND status='indexed'`,
			path, fileSize, mtime,
		).Scan(&existingID); err == nil {
			if pendingAlbumID != "" && ix.albumAssigner != nil {
				ix.albumAssigner(existingID, pendingAlbumID)
			}
			return true
		}
	}

	// 3. Streaming SHA-256 (os.Open + io.Copy): compute while reading, never
	// holding the whole file in memory — a few dozen KB image and a
	// multi-GB video pay the same constant-size memory cost.
	checksum, err := sha256FileStream(path)
	if err != nil {
		return false
	}

	// 4. Short-circuit when the checksum matches an already-indexed record.
	// This dedup logic is semantically identical to before the change —
	// it just no longer requires "the whole file has been read into
	// memory". It's the fallback for the stat fast-path above: legacy data
	// whose mtime hasn't been backfilled yet, or a file that's been
	// "touched" without its content changing, both get recognized here as
	// "actually already processed".
	// Records with status='pending' (e.g. left by a crash) are intentionally
	// re-processed so they can reach 'indexed' status.
	// When opts.force is set, bypass this short-circuit entirely.
	if !opts.force {
		var existingID string
		err = ix.db.QueryRow(`SELECT id FROM assets WHERE checksum=? AND status='indexed'`, checksum).Scan(&existingID)
		if err == nil {
			// Critical backfill: rows written before this upgrade have
			// mtime=NULL, so step 2's stat fast path will always miss and
			// every rescan has to stream-read the whole file again here.
			// Now that we've confirmed this path's content hasn't changed
			// (checksum hit an indexed row), write size+mtime back onto
			// this file_path so the next rescan hits step 2 with zero
			// reads, fully escaping "legacy rows get the whole file
			// re-read on every restart". Only updates when this exact path
			// actually has an indexed row (pure content dedup — a
			// different new path with the same content has no
			// corresponding row here — is a 0-row no-op; that case has no
			// row of its own to backfill and is unrelated to this fix).
			if _, uerr := ix.db.Exec(
				`UPDATE assets SET mtime=?, file_size=? WHERE file_path=? AND status='indexed' AND (mtime IS NULL OR mtime<>?)`,
				mtime, fileSize, path, mtime,
			); uerr != nil {
				fmt.Fprintf(os.Stderr, "[indexer] mtime backfill failed %s: %v\n", path, uerr)
			}
			// already fully indexed — assign to album if requested, then short-circuit
			if pendingAlbumID != "" && ix.albumAssigner != nil {
				ix.albumAssigner(existingID, pendingAlbumID)
			}
			return true
		}
	}

	// 5. Detect MIME type and decide image vs. video. Known extensions go
	// straight to the table without touching disk; only unknown extensions
	// read the file header for content sniffing (detectMimeType).
	ext := strings.ToLower(filepath.Ext(path))
	mime := detectMimeType(path, ext)
	isVideo := strings.HasPrefix(mime, "video/") || videoExts[ext]

	// 6. Gather metadata.
	var takenAt time.Time
	var durationMs int64
	var exifResult *exif.Result
	var mediaInfo *ffmpeg.MediaInfo
	var keyframePath string
	var keyframeTmpDir string
	// data is only populated on the image path, and only when the file
	// doesn't exceed maxImageReadBytes; the video path leaves it nil
	// throughout — keyframe extraction/probing both go through ffmpeg by
	// path, never touching data.
	var data []byte

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

		// Images larger than maxImageReadBytes skip reading the whole image
		// into memory: ML faceData stays empty (step 9's CLIP below falls
		// back to the thumbnail only, and skips entirely if there's no
		// thumbnail; OCR is skipped outright), but EXIF (already parsed
		// via streaming), thumbnails (thumb.Generate reads by path), and
		// the DB write all complete as usual — an abnormally huge image
		// shouldn't hold back basic indexing.
		if imageExceedsReadLimit(fileSize) {
			zap.L().Warn("image exceeds indexing read limit, skipping ML that depends on original bytes (face detection/OCR); CLIP still uses the thumbnail, basic indexing proceeds as usual",
				zap.String("path", path),
				zap.Int64("file_size", fileSize),
				zap.Int64("limit_bytes", maxImageReadBytes))
		} else if b, rerr := os.ReadFile(path); rerr == nil {
			data = b
		}

		// Most JPEGs put dimensions in the SOF marker rather than EXIF.
		// Fall back to image.DecodeConfig (header-only decode) when EXIF lacks them.
		if exifResult != nil && (exifResult.Width == 0 || exifResult.Height == 0) && len(data) > 0 {
			if cfg, _, derr := image.DecodeConfig(bytes.NewReader(data)); derr == nil {
				exifResult.Width = cfg.Width
				exifResult.Height = cfg.Height
			}
		}
	}

	// 7. INSERT into assets with status='pending'.
	// fileSize/mtime reuse step 1's stat result, no repeat os.Stat.
	assetID := uuid.NewString()
	// Fall back to file mtime when no embedded capture time was found.
	// Without this, files lacking EXIF DateTime or video creation_time would
	// all collapse to indexed_at and bunch together on the timeline.
	if takenAt.IsZero() {
		takenAt = fi.ModTime()
	}
	originalName := filepath.Base(path)

	_, err = ix.db.Exec(`
		INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
		                   taken_at, duration_ms, is_live_photo_video, status, checksum, mtime)
		VALUES(?,?,?,?,?,?,?,0,'pending',?,?)
		ON CONFLICT(file_path) DO UPDATE SET
		  checksum      = excluded.checksum,
		  file_size     = excluded.file_size,
		  mime_type     = excluded.mime_type,
		  original_name = excluded.original_name,
		  taken_at      = excluded.taken_at,
		  duration_ms   = excluded.duration_ms,
		  status        = 'pending',
		  mtime         = excluded.mtime,
		  face_scanned  = CASE WHEN excluded.checksum <> checksum THEN 0 ELSE face_scanned END,
		  caption_synced = CASE WHEN excluded.checksum <> checksum THEN 0 ELSE caption_synced END`,
		// face_scanned is only reset to 0 when the content actually changed
		// (checksum differs), handing it back to RunPipeline for
		// re-detection; a pure force rerun (e.g. Embedder/Rebuilder's CLIP
		// backfill over unchanged content, same checksum) must not clear an
		// already-completed face-detection flag — otherwise every CLIP
		// backfill pass would throw the same batch of assets back into the
		// face-detection queue, producing duplicate face_detections rows.
		// caption_synced follows the same semantics: only reset to 0 when
		// content actually changed, handing it back to the photo-knowledge
		// feed pipeline to re-hand off to Parser; a backfill over unchanged
		// content doesn't clear the already-handed-off flag, avoiding
		// duplicate feeds.
		assetID, path, fileSize, mime, originalName,
		nullTime(takenAt), sqlNullInt64(durationMs),
		checksum, mtime,
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

	// 8. INSERT/UPDATE asset_exif — images and videos both write their metadata.
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

	// 9. Generate thumbnails.
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

	// A video pregenerates its hover-preview sprite (sprite.jpg, a few
	// hundred KB) asynchronously as soon as it's indexed, eliminating the
	// on-demand generation gap on first hover. preview.mp4 (a low-bitrate
	// preview video, tens of MB apiece) defaults to pure lazy generation —
	// only pregenerated here when photos.PreviewPregen=true; otherwise left
	// to the /preview route's on-demand generation (see route/v1/assets.go
	// Preview), so disk isn't spent on videos that are never previewed.
	// Best-effort: the goroutine blocks queuing on the generator's
	// semaphore (concurrency ≤2, shared by both artifact kinds); failures
	// are only logged. The Ensure core is idempotent (returns instantly if
	// already present) and dedups in-flight, safe to run concurrently with
	// the /sprite, /preview routes and the startup backfill. sprite still
	// requires dur>0 (the fps expression needs a duration); preview has no
	// such dependency, so its trigger condition is relaxed to just isVideo.
	if isVideo {
		previewPath := filepath.Join(ix.thumbDir, assetID, "preview.mp4")
		spritePath := filepath.Join(ix.thumbDir, assetID, "sprite.jpg")
		pregen := ix.previewPregen
		go func(src, previewOut, spriteOut string, dur int64, id string) {
			if dur > 0 {
				if _, err := ix.sprites.Ensure(src, spriteOut, dur); err != nil {
					zap.L().Warn("sprite pregeneration failed", zap.String("asset_id", id), zap.Error(err))
				}
			}
			if pregen {
				if err := ix.sprites.EnsurePreview(src, previewOut); err != nil {
					zap.L().Warn("preview pregeneration failed", zap.String("asset_id", id), zap.Error(err))
				}
			}
		}(path, previewPath, spritePath, durationMs, assetID)
	}

	// 10. ML inference (only when ML service is ready).
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
		// CLIP embedding (skipped when ScenesEnabled is off — note the
		// embedding also underpins semantic search, so new photos won't
		// participate in semantic search once it's off).
		if config.Cfg == nil || config.Cfg.ScenesEnabled {
			// A failure doesn't block ingestion (thumbnail/EXIF proceed as
			// usual), but it must leave a trace: an asset missing its
			// vector is completely unreachable by semantic search, relying
			// on the end-of-batch/ML-recovery-chain Backfill to fill it in.
			if err := ix.embedClip(assetID, faceData); err != nil {
				zap.L().Warn("CLIP embedding failed, relying on Backfill",
					zap.String("asset_id", assetID), zap.Error(err))
			}
		}

		if len(faceData) > 0 {
			// Face detection + recognition has been handed off to the
			// independent FaceService.RunPipeline task (detection 0→95% +
			// clustering tail 95→100%, real progress): new photos land in
			// the library first and are visible, with person tagging
			// arriving seconds to minutes later, trading a faster
			// ingestion for real progress reporting. No inline detection
			// here anymore.

			// OCR uses the same full-detail input as faces (original photo or
			// full keyframe) — small text on receipts/documents is lost at
			// thumbnail resolution.
			// Videos don't run OCR: running OCR on a video keyframe is
			// meaningless, and would misclassify "screen recording /
			// contains text" videos into the "OCR/document" category
			// (asset_ocr having a hit is what triggers that category).
			// Videos only get CLIP for visual search; true video
			// understanding (segmented embedding) is future work.
			// OCR (skipped when OCREnabled is off, or for videos).
			if !isVideo && (config.Cfg == nil || config.Cfg.OCREnabled) {
				ocrData := faceData
				if oversizedForML(ocrData) {
					// The original image exceeds immich-ml/PIL's 178.9MP
					// hard cap safety margin (maxMLInputPixels), so a
					// /predict request would necessarily 500 — fall back to
					// the large.jpg thumbnail already generated in step 8
					// above instead of the original.
					if thumb := readLargeOrSmallThumb(ix.thumbDir, assetID); len(thumb) > 0 {
						ocrData = thumb
					} else {
						zap.L().Warn("original image exceeds ML pixel limit and no thumbnail is available, skipping OCR",
							zap.String("asset_id", assetID))
						ocrData = nil
					}
				}
				if len(ocrData) > 0 {
					if err := ix.ocrAsset(assetID, ocrData); err != nil {
						fmt.Fprintf(os.Stderr, "[indexer] OCR failed for %s: %v\n", assetID, err)
					} else if derr := ix.computeDocVerdict(assetID); derr != nil {
						fmt.Fprintf(os.Stderr, "[indexer] doc verdict failed for %s: %v\n", assetID, derr)
					}
				}
			}
		}
	}

	// 11. Mark as indexed.
	if _, err := ix.db.Exec(`
		UPDATE assets SET status='indexed', indexed_at=? WHERE id=?`,
		time.Now(), assetID,
	); err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] failed to mark asset indexed %s: %v\n", assetID, err)
		return false
	}
	if ix.onIndexed != nil {
		go ix.onIndexed(assetID) // async side channel: a feed failure doesn't affect the indexing result
	}
	return true
}

// insertFaceDetections writes ML-detected faces for assetID into
// face_detections. Extracted from processFileInternal's original inline loop
// so FaceService.RunPipeline's detection stage can reuse the exact same write
// path (id/bbox/embedding shape, FaceDim guard, best-effort error logging).
func insertFaceDetections(db *sql.DB, assetID string, faces []mlclient.FaceResult) {
	for _, face := range faces {
		if len(face.Embedding) != common.FaceDim {
			continue
		}
		bboxJSON, _ := json.Marshal(face.BBox)
		faceID := uuid.NewString()
		if _, err := db.Exec(
			`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
			faceID, assetID, string(bboxJSON), sqlite.SerializeFloat32(face.Embedding),
		); err != nil {
			fmt.Fprintf(os.Stderr, "[indexer] failed to insert face_detection %s: %v\n", assetID, err)
		}
	}
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
// OCR'd". Kept lines are also written one-per-row into asset_ocr_lines with
// their normalized quadrilaterals (search-hit highlighting); boxes_ver=1
// marks the geometry as stored so the backfill skips this asset.
func (ix *Indexer) ocrAsset(assetID string, imageData []byte) error {
	lines, err := ix.ml.OCR(imageData)
	if err != nil {
		return err
	}
	kept := make([]mlclient.OCRLine, 0, len(lines))
	texts := make([]string, 0, len(lines))
	coverage := 0.0
	for _, l := range lines {
		if l.Score >= minOCRScore && strings.TrimSpace(l.Text) != "" {
			kept = append(kept, l)
			texts = append(texts, l.Text)
			coverage += quadArea(l.Box)
		}
	}
	if coverage > 1 {
		coverage = 1
	}

	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`
		INSERT INTO asset_ocr(asset_id, text, coverage, line_count, boxes_ver, ocr_at)
		VALUES(?,?,?,?,1,CURRENT_TIMESTAMP)
		ON CONFLICT(asset_id) DO UPDATE SET
		  text=excluded.text, coverage=excluded.coverage,
		  line_count=excluded.line_count, boxes_ver=1, ocr_at=excluded.ocr_at,
		  doc_ver=0`,
		assetID, strings.Join(texts, "\n"), coverage, len(texts)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM asset_ocr_lines WHERE asset_id = ?`, assetID); err != nil {
		return err
	}
	for i, l := range kept {
		boxJSON := []byte("[]")
		if len(l.Box) == 8 {
			if b, merr := json.Marshal(l.Box); merr == nil {
				boxJSON = b
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO asset_ocr_lines(asset_id, line_no, text, box, score)
			VALUES(?,?,?,?,?)`,
			assetID, i, l.Text, string(boxJSON), l.Score); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		// Vector is now persisted: score aesthetics inline. A failure is
		// only logged and doesn't affect the vector write result.
		ix.scoreAesthetic(assetID, vec)
		return nil
	}
	if _, err := ix.db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, blob); err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] insert clip_embeddings %s: %v\n", assetID, err)
		return err
	}
	// Vector is now persisted: score aesthetics inline. A failure is only
	// logged and doesn't affect the vector write result.
	ix.scoreAesthetic(assetID, vec)
	return nil
}

// SetAestheticHead injects the aesthetic-scoring head; nil disables inline scoring.
func (ix *Indexer) SetAestheticHead(h *aesthetic.Head) { ix.aestheticHead = h }

// scoreAesthetic computes the aesthetic score for an asset whose vector has
// already been written. Silently skipped when the head isn't injected or the
// dimension doesn't match (NaN).
func (ix *Indexer) scoreAesthetic(assetID string, vec []float32) {
	if ix.aestheticHead == nil {
		return
	}
	s := ix.aestheticHead.Score(vec)
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return
	}
	if _, err := ix.db.Exec(`UPDATE assets SET aesthetic_score=? WHERE id=?`, s, assetID); err != nil {
		zap.L().Warn("aesthetic: failed to write score", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// sha256File returns the hex-encoded SHA-256 hash of data. processFileInternal's
// main dedup path has switched to sha256FileStream below (which doesn't
// require the whole file to be read into memory); this version is kept for
// cases that already have the bytes in memory (e.g. cross-checking against
// the streaming hash result in tests).
func sha256File(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sha256FileStream computes the file's SHA-256 while reading (os.Open +
// io.Copy), without needing to hold the whole file in memory — an 8GB video
// and a few dozen KB thumbnail pay the same constant-size (io.Copy's
// internal buffer size) resident memory; this is the core mechanism by which
// processFileInternal eliminates OOM.
func sha256FileStream(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkSupported recursively walks dir, calling fn for each supported media file.
// Directories whose names begin with "." (e.g. .trash) are skipped entirely so
// that soft-deleted files are never re-indexed. ctx is checked before each
// filesystem entry is visited; if it is cancelled, the walk stops immediately
// and returns ctx.Err() (context.Canceled / context.DeadlineExceeded).
//
// dir itself is rejected up front if it sits under a ".snapshots" directory
// component (isInSnapshotsDir) — this covers callers that hand walkSupported
// a root nested inside .snapshots directly (e.g. a stale/manually supplied
// path), even though the normal source of such roots (mount enumeration) is
// already filtered at IsExcludedMount. Once inside the walk, any directory
// literally named ".snapshots" is skipped unconditionally (not just when
// path != dir, unlike the generic hidden-dir rule below) so a walk rooted
// one level above .snapshots never descends into it.
func walkSupported(ctx context.Context, dir string, fn func(path string)) error {
	if isInSnapshotsDir(dir) {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err() // WalkDir aborts the whole walk on any non-SkipDir error
		default:
		}
		if err != nil {
			// Merged in from the 2026-07-06 plan02 review: an unreadable
			// single entry (permissions / raced deletion / dangling
			// symlink) only skips that subtree, it doesn't abort the whole
			// walk — previously `return err` here made every file sorted
			// after the bad entry miss this scan/live-indexing pass; an
			// error on the root directory itself is still propagated so
			// the caller is aware.
			if path == dir {
				return err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == snapshotsDirName {
				return filepath.SkipDir
			}
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
	if err := walkSupported(context.Background(), dir, func(p string) { paths = append(paths, p) }); err != nil {
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
			Label:     "Indexing photos",
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
			Label:     "Indexing photos",
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
				Label:     "Indexing photos",
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

// ScanDirectoryOnce runs ScanDirectory(dir) unless a scan for the same dir is
// already in flight (the watcher's mount polling and MountGuard's replug
// recovery can both trigger a rescan for the same mount at the same time).
// Returns started=false to indicate it was skipped due to dedup.
func (ix *Indexer) ScanDirectoryOnce(dir string) (bool, error) {
	if _, loaded := ix.scanDirInFlight.LoadOrStore(dir, struct{}{}); loaded {
		return false, nil
	}
	defer ix.scanDirInFlight.Delete(dir)
	return true, ix.ScanDirectory(dir)
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

// pruneVideoOCR deletes OCR rows for videos. Videos no longer run OCR
// (keyframe OCR is meaningless, and would misclassify videos with on-screen
// text into the "OCR/document" category); this cleans up legacy video OCR
// rows at startup so already-indexed videos also drop out of the OCR
// category immediately. Idempotent: videos no longer produce asset_ocr rows
// going forward.
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
	if ix.onCaptionDelete != nil {
		ix.onCaptionDelete(id) // caption hook: keeps the agent from retrieving ghost results
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
	if len(gone) > 0 && !pruneDeleteAllowed(dir, ix.containingMountRoot) {
		zap.L().Warn("pruneMissingUnder: mount state changed during scan, aborting prune",
			zap.String("dir", dir), zap.Int("wouldDelete", len(gone)))
		return nil
	}
	for _, r := range gone {
		dropClipVector(ix.db, r.id) // before the cascade drops asset_clip_idx
		if ix.onCaptionDelete != nil {
			ix.onCaptionDelete(r.id) // caption hook: keeps the agent from retrieving ghost results
		}
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

// pruneDeleteAllowed re-validates right before the destructive pass:
// A removable drive can be unplugged during the stat loop, at which point
// every file fails stat; without this re-check, an entire subtree's assets/
// vectors/thumbnails would be mass-deleted by mistake.
//
// The re-check target is the **mount root** dir belongs to, not dir itself:
// dir being deleted wholesale (Files deleting an album folder) is exactly
// the legitimate case prune is meant to handle, and stat dir would
// necessarily fail — using that as the criterion would misjudge a
// legitimate deletion as a drive unplug and leave it stuck in the index
// forever. A genuine unplug is caught by two checks instead — containingRoot
// can't find a root once the mount has vanished from /proc/mounts; stat on
// the mount root errors out (EIO, etc.) when a dead mount lingers in the
// mount table.
func pruneDeleteAllowed(dir string, containingRoot func(string) (string, bool)) bool {
	root, ok := containingRoot(dir)
	if !ok {
		return false
	}
	if _, err := os.Stat(root); err != nil {
		return false
	}
	return true
}

// containingMountRoot returns the currently-mounted scan root that dir equals
// or lives under. Roots always include /DATA, so library directories on the
// system disk are always eligible; a /media/* mount that has vanished from
// the mount table is not.
func (ix *Indexer) containingMountRoot(dir string) (string, bool) {
	mounts := ix.mountRoots
	if mounts == nil {
		mounts = EnumerateScanRoots
	}
	cleaned := strings.TrimRight(dir, string(filepath.Separator))
	best := ""
	for _, root := range mounts() {
		r := strings.TrimRight(root, string(filepath.Separator))
		if (cleaned == r || strings.HasPrefix(cleaned, r+string(filepath.Separator))) && len(r) > len(best) {
			best = r
		}
	}
	return best, best != ""
}

// dirUnderMountedRoot reports whether dir is one of (or lives under one of)
// the currently-mounted scan roots.
func (ix *Indexer) dirUnderMountedRoot(dir string) bool {
	_, ok := ix.containingMountRoot(dir)
	return ok
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

// spriteCandidate is one row of the sprite-backfill candidate set: an indexed,
// non-deleted video with a known duration.
type spriteCandidate struct {
	id         string
	filePath   string
	durationMs int64
}

// spriteBackfillCandidates selects existing videos still missing a hover-preview
// sprite. Pure SQL filtering only — no stat, no generation — so it can be
// exercised independently of ffmpeg/filesystem state in tests.
func spriteBackfillCandidates(db *sql.DB) ([]spriteCandidate, error) {
	rows, err := db.Query(`
		SELECT id, file_path, COALESCE(duration_ms,0) FROM assets
		WHERE mime_type LIKE 'video/%' AND status='indexed' AND deleted_at IS NULL
		  AND COALESCE(duration_ms,0) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []spriteCandidate
	for rows.Next() {
		var c spriteCandidate
		if err := rows.Scan(&c.id, &c.filePath, &c.durationMs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// pendingBackfill returns candidates missing a hover-preview artifact.
// When includePreview=false (i.e. PreviewPregen off), only sprite.jpg's
// presence is checked; preview.mp4 is left to lazy generation.
func pendingBackfill(candidates []spriteCandidate, thumbDir string, includePreview bool) []spriteCandidate {
	var pending []spriteCandidate
	for _, c := range candidates {
		_, spriteErr := os.Stat(filepath.Join(thumbDir, c.id, "sprite.jpg"))
		previewMissing := false
		if includePreview {
			_, perr := os.Stat(filepath.Join(thumbDir, c.id, "preview.mp4"))
			previewMissing = perr != nil
		}
		if spriteErr != nil || previewMissing {
			pending = append(pending, c)
		}
	}
	return pending
}

// BackfillSprites backfills sprites for existing videos, and (only when
// photos.PreviewPregen=true) preview videos too (called once at startup,
// also re-triggered by the batch-done hook). CAS-guarded against
// re-entrancy; generates one at a time (the generator's semaphore also
// imposes a global concurrency≤2 cap, shared by both artifact kinds);
// gives up on the whole pass immediately when ffmpeg is missing
// (exec.ErrNotFound), instead of spamming an error log per item. The
// candidate query still filters on duration_ms>0 (a broken video with
// unknown duration is extremely rare, and is left to the route's lazy
// fallback); both artifact kinds are checked for existence in the loop body
// via their own os.Stat (saves a function call, matches sprite's existing
// style; the preview side's ensure core is also naturally idempotent).
// When PreviewPregen=false (the default), preview is left entirely to the
// /preview route's lazy generation, and this function only backfills sprite.
//
// Task-bar integration (following faces.go RunPipeline's lifecycle
// pattern): first pre-scans each candidate for missing sprite.jpg/
// preview.mp4, and only fires the "Generating video previews" task when
// there's real backlog (total>0) — a single upload's inline pregeneration
// completes in seconds and won't be caught here (already complete by
// prescan time), preserving the existing behavior of not firing a task for
// that case. current is incremented and Upserted after each candidate is
// processed (whether generated, skipped, or reused via join), with the
// registry throttling its own publish rate. When ffmpeg is missing, the
// whole pass gives up and marks the task's terminal state as error;
// individual video failures don't abort the pass, matching BackfillOCR's
// existing convention (see embedder.go backfillOCROnce): the terminal state
// is only marked error (TaskErrPreviewPartialFailed) when every candidate
// failed, otherwise (including partial failures) the terminal state is
// done, with the failure count folded into the summary log — a
// permanently-corrupt video would fail every single backfill pass, and
// treating "any failure occurred" as error would make the task bar flash
// Failed every round as noise. No terminal state is published when ctx is
// cancelled; the task is left running, for the registry's stale-task sweep
// to clean up — consistent with how faces.go RunPipeline handles
// interruption.
func (ix *Indexer) BackfillSprites(ctx context.Context) {
	if !ix.spriteBackfillRunning.CompareAndSwap(false, true) {
		return
	}
	defer ix.spriteBackfillRunning.Store(false)

	candidates, err := spriteBackfillCandidates(ix.db)
	if err != nil {
		zap.L().Warn("sprite backfill candidate query failed", zap.Error(err))
		return
	}

	// Pre-scan: for each candidate, check whether sprite.jpg (and
	// preview.mp4, when PreviewPregen is on) is missing; only counts as a
	// pending item if either is missing, giving us total. Candidates where
	// both are already present (or sprite alone, when PreviewPregen is off)
	// aren't counted in total and don't enter the processing loop below
	// (there's nothing to do for them this round). Returns immediately
	// without firing a task when total==0.
	pending := pendingBackfill(candidates, ix.thumbDir, ix.previewPregen)
	total := int64(len(pending))
	if total == 0 {
		return
	}

	taskID := fmt.Sprintf("preview_%d", time.Now().UnixNano())
	started := time.Now()
	pub := func(current int64, status, errKey string, errParams map[string]string) {
		if ix.taskReg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "preview-backfill",
			Label:     "Generating video previews",
			Current:   current,
			Total:     total,
			Progress:  float64(current) / float64(total),
			Status:    status,
			StartedAt: started,
		}
		if errKey != "" {
			t.SetError(errKey, errParams)
		}
		ix.taskReg.Upsert(t)
	}
	// scheduleRemove handles terminal-state cleanup: following faces.go's
	// pattern, wait taskCleanupDelay after done/error before removing from
	// the registry, giving the frontend a window to display the terminal state.
	scheduleRemove := func() {
		go func() {
			time.Sleep(taskCleanupDelay)
			if ix.taskReg != nil {
				ix.taskReg.Remove(taskID)
			}
		}()
	}

	pub(0, "running", "", nil)

	// sourceMissing only counts candidates skipped entirely because the
	// "source video file is missing" (os.Stat failed); whether sprite.jpg /
	// preview.mp4 already exist is checked independently via os.Stat below
	// and isn't counted in this counter (corresponding to the implicit
	// semantics of spritesGenerated / previewsGenerated not incrementing).
	var spritesGenerated, previewsGenerated, sourceMissing int
	var current, failed int64
	for _, c := range pending {
		if ctx.Err() != nil {
			return
		}
		if _, statErr := os.Stat(c.filePath); statErr != nil {
			sourceMissing++
			current++
			pub(current, "running", "", nil)
			continue
		}

		var itemFailed bool

		spritePath := filepath.Join(ix.thumbDir, c.id, "sprite.jpg")
		if _, statErr := os.Stat(spritePath); statErr != nil {
			if _, err := ix.sprites.Ensure(c.filePath, spritePath, c.durationMs); err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					zap.L().Warn("ffmpeg unavailable, giving up on this sprite/preview backfill pass", zap.Error(err))
					pub(current, "error", TaskErrPreviewFfmpegMissing, nil)
					scheduleRemove()
					return
				}
				zap.L().Warn("sprite backfill failed", zap.String("asset_id", c.id), zap.Error(err))
				itemFailed = true
			} else {
				spritesGenerated++
			}
		}

		if ix.previewPregen {
			previewPath := filepath.Join(ix.thumbDir, c.id, "preview.mp4")
			if _, statErr := os.Stat(previewPath); statErr != nil {
				if err := ix.sprites.EnsurePreview(c.filePath, previewPath); err != nil {
					if errors.Is(err, exec.ErrNotFound) {
						zap.L().Warn("ffmpeg unavailable, giving up on this sprite/preview backfill pass", zap.Error(err))
						pub(current, "error", TaskErrPreviewFfmpegMissing, nil)
						scheduleRemove()
						return
					}
					zap.L().Warn("preview backfill failed", zap.String("asset_id", c.id), zap.Error(err))
					itemFailed = true
				} else {
					previewsGenerated++
				}
			}
		}

		if itemFailed {
			failed++
		}
		current++
		pub(current, "running", "", nil)
	}
	if spritesGenerated > 0 || previewsGenerated > 0 || failed > 0 {
		zap.L().Info("sprite/preview backfill complete",
			zap.Int("sprites_generated", spritesGenerated),
			zap.Int("previews_generated", previewsGenerated),
			zap.Int("source_missing", sourceMissing),
			zap.Int64("failed", failed))
	}

	if failed > 0 && failed == total {
		// Matches BackfillOCR's convention (backfillOCROnce): the whole
		// batch is only judged error when every candidate failed;
		// individual failures (even recurring ones, e.g. a permanently
		// corrupt video) are only logged, and the terminal state stays
		// done as usual, avoiding the task bar flashing Failed every round
		// as noise.
		pub(current, "error", TaskErrPreviewPartialFailed, map[string]string{"failed": strconv.FormatInt(failed, 10)})
	} else {
		pub(current, "done", "", nil)
	}
	scheduleRemove()
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
	// deleted_at IS NULL: a trashed asset that also sits on an unplugged drive
	// is already gone from the library view; it must not inflate the
	// "N photos are on a disconnected drive" hint.
	_ = ix.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE offline = 1 AND deleted_at IS NULL`).Scan(&s.Offline)
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
