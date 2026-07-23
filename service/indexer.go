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

// mimeSniffBytes 是未知扩展名回退到内容嗅探时最多读取的文件头部字节数。
// http.DetectContentType 本身只看前 512B，这里留一点冗余；已知扩展名
// （canonicalMime 命中）完全不受这个常量影响，压根不会读文件。
const mimeSniffBytes = 4096

// readHeader 最多读取 path 的前 n 个字节，用于只需要文件头部信息的场景（如
// MIME 内容嗅探），避免像 os.ReadFile 那样把整个文件读入内存。文件本身小于
// n 字节是正常情况（返回已读到的部分，不算错误）。
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

// detectMimeType 是 processFileInternal 里实际调用的 MIME 探测入口：已知扩展
// 名直接查 canonicalMime 表、完全不碰磁盘；只有未识别的扩展名才读文件头部
// （mimeSniffBytes）做内容嗅探——取代原来"先把整个文件读进内存，再嗅探"的
// 做法（视频文件哪怕几个 GB，MIME 探测阶段现在最多只读 4KB）。
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

// maxImageReadBytes 是索引阶段把图片原图整个读入内存（供 ML faceData 与
// image.DecodeConfig 尺寸兜底使用）的字节上限，防止异常超大/伪装的图片文件
// 把 Go 进程常驻内存打爆。注意这和 oversizedForML（mlinput.go，178.9MP 像素
// 上限，管的是喂给 immich-ml /predict 的输入尺寸）是两码事，不要混用：那个
// 判定图片"能不能喂给 ML"，这个判定图片"要不要整图读进内存"。
// 声明成 var 而不是 const 是为了让测试能注入一个更小的阈值，不必真的在测试
// 里落地一个 100MB+ 的文件。
var maxImageReadBytes int64 = 100 * 1024 * 1024 // 100MB

// imageExceedsReadLimit 是纯函数判定：文件大小是否超过 maxImageReadBytes。
// 抽出来单独测试边界值，不需要构造真实的大文件。
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

	// scanDirInFlight 对整目录级补扫做去重：watcher 挂载轮询(followMounts)与
	// MountGuard 插回恢复都可能对同一挂载触发补扫，同一 dir 只允许一份
	// ScanDirectory 在跑，避免重复全量扫描徒耗 IO（见 ScanDirectoryOnce）。
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

	// doc 分类的提示词向量进程内缓存(见 docverdict.go loadPromptVecs)。
	promptMu    sync.Mutex
	promptDoc   [][]float32
	promptPhoto [][]float32

	// aestheticHead 非 nil 时,writeClipEmbedding 成功后内联计算美学分写库
	// (纯本地矩阵乘,微秒级)。经 SetAestheticHead 注入,AestheticEnabled=false 时为 nil。
	aestheticHead *aesthetic.Head

	// sprites 是进程内共享的悬浮预览雪碧图生成器：索引管线内联预生成、启动
	// 补跑（BackfillSprites）与 /sprite 路由的现场生成必须共用同一实例，
	// 其 in-flight 去重才能防止并发 ffmpeg 写同一输出文件。
	sprites *SpriteGenerator

	// spriteBackfillRunning 是 BackfillSprites 的 CAS 重入门闩：一次只允许
	// 一轮存量补跑在跑，避免服务重启风暴或误触发导致多轮并发扫同一批候选。
	spriteBackfillRunning atomic.Bool

	// onIndexed 在资产写为 status='indexed'（唯一写入点，见 processFileInternal
	// 末尾）成功后异步调用一次，供 CaptionFeeder.FeedOne 内联投喂钩子使用。
	// 函数字段注入（同 albumAssigner/onBatchDone 模式），避免 Indexer 直接依赖
	// CaptionFeeder 类型；为 nil 时（未接线 / 测试）安全跳过。
	onIndexed func(assetID string)

	// onCaptionDelete 在硬删除资产（RemoveByPath/pruneMissingUnder，紧邻
	// dropClipVector 调用点）成功后调用，供 CaptionFeeder.DeleteRemote 联动使
	// 用（Task 4）。函数字段注入，同 onIndexed；为 nil 时安全跳过。
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
// broader scan may have indexed. mounts 由调用方传 enumerateRcloneMounts(),
// 注入参数便于测试;未挂载的云盘不猜路径模式、不动。
// 挂载点名含 `_`(rclone 命名 /mnt/<user>_<provider>_<id>)是 LIKE 单字符
// 通配,必须用 substr 前缀比较,不能用 LIKE。
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
		sprites:      NewSpriteGenerator(),
	}
}

// Sprites 返回进程内共享的雪碧图生成器：索引内联预生成、启动补跑与
// /sprite 路由必须共用同一实例，其 in-flight 去重才能防止并发 ffmpeg
// 写同一输出文件。
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

// SetCaptionDelete 注入硬删除资产成功后的 caption 删除回调（通常是
// CaptionFeeder.DeleteRemote），供 RemoveByPath/pruneMissingUnder 调用（Task 4）。
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

	// 1. 先 stat，不读一个字节。size+mtime 既是下面 P2 快速跳过判断的依据，也
	// 直接复用为 INSERT 阶段需要的 file_size/mtime，避免处理期间文件被替换导
	// 致前后两次 stat 结果不一致。
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileSize := fi.Size()
	mtime := fi.ModTime().UnixNano()

	// 2. stat 快速跳过（P2）：已经 status='indexed' 且 file_size+mtime 都没变
	// 的文件，不读一个字节就能确认"这份内容早就处理过"——这是打破"重启→整读
	// 全部 pending 行→再次 OOM→再被杀重启"死循环的关键一步。旧数据（升级前
	// 入库、mtime 列还是 NULL）在这里必然 miss，会往下走一遍流式哈希 + checksum
	// 判重，并在第 4 步命中 checksum 短路时就地把 mtime 回填到本 file_path，
	// 下次重启/续扫即可直接命中这里、彻底免读。
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

	// 3. 流式计算 SHA-256（os.Open + io.Copy）：边读边算，不把整个文件驻留到
	// 内存里——几十 KB 的图片和几个 GB 的视频走的是同一份常量级内存开销。
	checksum, err := sha256FileStream(path)
	if err != nil {
		return false
	}

	// 4. checksum 命中已索引记录时短路。这条判重逻辑和改动前语义完全一致，
	// 只是不再要求"整个文件已经读进内存"——它是上面 stat 快速路径的兜底：
	// mtime 还没回填的存量数据、或者文件被"touch"过但内容没变的场景，都靠它
	// 兜底识别成"其实早就处理过"。
	// Records with status='pending' (e.g. left by a crash) are intentionally
	// re-processed so they can reach 'indexed' status.
	// When opts.force is set, bypass this short-circuit entirely.
	if !opts.force {
		var existingID string
		err = ix.db.QueryRow(`SELECT id FROM assets WHERE checksum=? AND status='indexed'`, checksum).Scan(&existingID)
		if err == nil {
			// 关键回填：升级前入库的存量行 mtime 为 NULL，第 2 步的 stat 快速
			// 路径永远 miss、每次续扫都要在这里重新流式读一遍整文件。既然已经
			// 确认这条路径的内容没变（checksum 命中 indexed），就把 size+mtime
			// 写回本 file_path，下次续扫第 2 步即可零读取命中，彻底摆脱"存量行
			// 每次重启都被整库重读一遍"。仅当本路径确有 indexed 行时才更新
			// （纯内容去重——同内容的另一个新路径此处无对应行——是 0 行 no-op，
			// 那种情况本就没有自己的行可回填，与本次修复无关）。
			if _, uerr := ix.db.Exec(
				`UPDATE assets SET mtime=?, file_size=? WHERE file_path=? AND status='indexed' AND (mtime IS NULL OR mtime<>?)`,
				mtime, fileSize, path, mtime,
			); uerr != nil {
				fmt.Fprintf(os.Stderr, "[indexer] mtime 回填失败 %s: %v\n", path, uerr)
			}
			// already fully indexed — assign to album if requested, then short-circuit
			if pendingAlbumID != "" && ix.albumAssigner != nil {
				ix.albumAssigner(existingID, pendingAlbumID)
			}
			return true
		}
	}

	// 5. Detect MIME type and decide image vs. video. 已知扩展名直查表，不碰
	// 磁盘；只有未知扩展名才读文件头部做内容嗅探（detectMimeType）。
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
	// data 只在图片路径、且文件不超过 maxImageReadBytes 时才会被填充；视频
	// 路径全程保持 nil——关键帧/probe 都按路径走 ffmpeg，一个字节都不碰 data。
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

		// 超过 maxImageReadBytes 的图片跳过整图读入内存：ML faceData 置空
		// （下面第 9 步 CLIP 会退化成只用缩略图、没有缩略图就跳过；OCR 直接
		// 跳过），但 EXIF（已流式解析）、缩略图（thumb.Generate 按路径读）、
		// 入库都照常完成——异常超大图不应该拖累基础索引。
		if imageExceedsReadLimit(fileSize) {
			zap.L().Warn("图片超过索引读取上限，跳过依赖原图字节的 ML（人脸检测/OCR），CLIP 仍走缩略图，基础索引照常",
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
	// fileSize/mtime 复用第 1 步的 stat 结果，不再重复 os.Stat。
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
		// face_scanned 只在内容真的变了(checksum 变化)才置回 0，交给 RunPipeline
		// 重新检测；纯粹的 force 重跑(如 Embedder/Rebuilder 对未变内容的 CLIP
		// 补跑,同一 checksum)不应清掉已完成的人脸检测标记——否则每轮 CLIP 补跑
		// 都会把同一批资产重新扔回人脸检测队列,产生重复的 face_detections 行。
		// caption_synced 同款语义：只在内容真的变了才置回 0，交给照片知识库
		// 投喂管线重新交接给 Parser；未变内容的补跑不清掉已交接标记，避免重复投喂。
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

	// 视频入库即异步预生成悬浮预览产物(雪碧图 + 低码率预览视频):消除首次
	// hover 的现场生成空窗。best-effort,goroutine 阻塞在生成器信号量(并发
	// ≤2,两类产物共享)上排队,失败只记日志;ensure 核心幂等(已存在秒退)
	// 且 in-flight 去重,与 /sprite、/preview 路由及启动补跑并发安全。
	// sprite 仍以 dur>0 为前提(fps 表达式需要时长);preview 无此依赖,启动
	// 条件放宽为 isVideo。
	if isVideo {
		previewPath := filepath.Join(ix.thumbDir, assetID, "preview.mp4")
		spritePath := filepath.Join(ix.thumbDir, assetID, "sprite.jpg")
		go func(src, previewOut, spriteOut string, dur int64, id string) {
			if dur > 0 {
				if _, err := ix.sprites.Ensure(src, spriteOut, dur); err != nil {
					zap.L().Warn("sprite 预生成失败", zap.String("asset_id", id), zap.Error(err))
				}
			}
			if err := ix.sprites.EnsurePreview(src, previewOut); err != nil {
				zap.L().Warn("preview 预生成失败", zap.String("asset_id", id), zap.Error(err))
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
		// CLIP embedding（ScenesEnabled 关闭时跳过——注意嵌入同时是语义搜索的基础，
		// 关闭后新照片不参与语义搜索）。
		if config.Cfg == nil || config.Cfg.ScenesEnabled {
			// 失败不阻断入库(缩略图/EXIF 照常),但必须留痕:缺向量的资产
			// 语义搜索完全搜不到,靠批次末尾/ML 恢复链的 Backfill 兜底补齐。
			if err := ix.embedClip(assetID, faceData); err != nil {
				zap.L().Warn("CLIP 嵌入失败,待 Backfill 兜底",
					zap.String("asset_id", assetID), zap.Error(err))
			}
		}

		if len(faceData) > 0 {
			// Face detection + recognition 已移交独立任务 FaceService.RunPipeline
			// （检测 0→95% + 聚类尾段 95→100%，真实进度）：新照片先入库可见，
			// 人物筛选晚几秒~几分钟，换真实进度与更快入库。此处不再内联检测。

			// OCR uses the same full-detail input as faces (original photo or
			// full keyframe) — small text on receipts/documents is lost at
			// thumbnail resolution.
			// 视频不跑 OCR:对视频关键帧做 OCR 没有实际意义,还会把「录屏/含文字画面」的
			// 视频误判进「OCR/文档」分类(asset_ocr 命中即归类)。视频只保留 CLIP 用于
			// 视觉检索;真正的视频理解(分段 embedding)是后续工作。
			// OCR（OCREnabled 关闭或视频时跳过）。
			if !isVideo && (config.Cfg == nil || config.Cfg.OCREnabled) {
				ocrData := faceData
				if oversizedForML(ocrData) {
					// 原图超过 immich-ml/PIL 的 178.9MP 硬上限的安全边际
					// (maxMLInputPixels),/predict 请求必然 500——降级用已在
					// 上面第 8 步生成好的 large.jpg 缩略图代替原图。
					if thumb := readLargeOrSmallThumb(ix.thumbDir, assetID); len(thumb) > 0 {
						ocrData = thumb
					} else {
						zap.L().Warn("原图超过 ML 像素上限且缩略图不可用，跳过 OCR",
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
		go ix.onIndexed(assetID) // 异步旁路：投喂失败不影响索引结果
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
		// 向量已落库:内联美学打分。失败只记日志不影响向量写入结果。
		ix.scoreAesthetic(assetID, vec)
		return nil
	}
	if _, err := ix.db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, blob); err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] insert clip_embeddings %s: %v\n", assetID, err)
		return err
	}
	// 向量已落库:内联美学打分。失败只记日志不影响向量写入结果。
	ix.scoreAesthetic(assetID, vec)
	return nil
}

// SetAestheticHead 注入美学评分头;nil 表示关闭内联打分。
func (ix *Indexer) SetAestheticHead(h *aesthetic.Head) { ix.aestheticHead = h }

// scoreAesthetic 对已写入向量的资产计算美学分。头未注入 / 维度不符(NaN)时静默跳过。
func (ix *Indexer) scoreAesthetic(assetID string, vec []float32) {
	if ix.aestheticHead == nil {
		return
	}
	s := ix.aestheticHead.Score(vec)
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return
	}
	if _, err := ix.db.Exec(`UPDATE assets SET aesthetic_score=? WHERE id=?`, s, assetID); err != nil {
		zap.L().Warn("aesthetic: 写分失败", zap.String("asset_id", assetID), zap.Error(err))
	}
}

// sha256File returns the hex-encoded SHA-256 hash of data. processFileInternal
// 的判重主路径已经改用下面的 sha256FileStream（不要求整文件读进内存）；这个
// 版本仍保留给已经拿到内存字节的场景（如测试里与流式哈希结果做交叉验证）。
func sha256File(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sha256FileStream 边读边算文件的 SHA-256（os.Open + io.Copy），不需要把整个
// 文件驻留到内存——8GB 的视频和几十 KB 的缩略图走的是同一份常量级（io.Copy
// 内部缓冲区大小）常驻内存，这是 processFileInternal 消除 OOM 的核心手段。
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
			return ctx.Err() // WalkDir 收到非 SkipDir 错误即整体终止
		default:
		}
		if err != nil {
			// 2026-07-06 plan02 审查并入:单个条目不可读(权限/竞态删除/悬空链接)
			// 只跳过该子树,不中止整棵遍历——此前 return err 会让排在坏条目之后的
			// 全部文件错过本轮扫描/实时索引;根目录本身出错仍上抛让调用方知晓。
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

// ScanDirectoryOnce runs ScanDirectory(dir) unless a scan for the same dir is
// already in flight (watcher 挂载轮询与 MountGuard 插回恢复可能同时对同一
// 挂载触发补扫)。返回 started=false 表示因去重而跳过。
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
	if ix.onCaptionDelete != nil {
		ix.onCaptionDelete(id) // caption 联动：防 agent 检索到幽灵结果
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
			ix.onCaptionDelete(r.id) // caption 联动：防 agent 检索到幽灵结果
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
// stat 循环期间可移动盘可能被拔出,届时所有文件都 stat 失败,若不复核
// 会把整棵子树的资产/向量/缩略图批量误删。
//
// 复核对象是 dir 所属的**挂载根**,而不是 dir 本身:dir 被整体删除(Files
// 删除相册文件夹)正是需要 prune 的合法场景,stat dir 必然失败,拿它当判据
// 会把合法删除误判成拔盘、永久滞留索引。真正的拔盘由两道检查兜住——挂载
// 从 /proc/mounts 消失时 containingRoot 拿不到根;死挂载残留在挂载表里时
// stat 挂载根会报错(EIO 等)。
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

// BackfillSprites 为存量视频补雪碧图与预览视频(启动时调用一次,批次完成钩子
// 也会追加触发)。CAS 防重入;顺序逐个生成(生成器信号量另有并发≤2 的全局上限,
// 两类产物共享);ffmpeg 不存在(exec.ErrNotFound)时立即放弃整轮,避免逐条刷
// 错误日志。候选查询仍以 duration_ms>0 过滤(时长未知的破损视频极罕见,交由
// 路由端惰性兜底),两类产物在循环体内各自 os.Stat 判存在跳过(省函数调用,
// 与 sprite 既有写法对齐;preview 侧 ensure 核心本身也天然幂等)。
//
// 任务栏接入(沿用 faces.go RunPipeline 的生命周期模式):先对候选逐条预扫描
// sprite.jpg/preview.mp4 是否缺失,只有真正有欠账(total>0)才发「生成视频
// 预览」任务——单条上传的内联预生成秒级完成,不会被这里捕获(prescan 时已经
// 齐备),维持不发任务的现状。current 每处理完一条候选(无论生成、跳过还是
// join 复用)就 +1 并 Upsert,由 registry 自身节流发布频率。ffmpeg 缺失时整
// 轮放弃并把任务标为 error 终态;个别视频生成失败不中断整轮,对齐 BackfillOCR
// 的既有惯例(见 embedder.go backfillOCROnce):只有全部候选都处理失败才把任务
// 终态标为 error(TaskErrPreviewPartialFailed),否则(含部分失败)终态为 done、
// 失败数计入汇总日志——一条永久损坏的视频每轮补跑都会失败,若按"出现失败就
// error"处理,任务栏会持续弹 Failed 造成噪音。ctx 取消时不发任何终态,任务留在
// running,交给 registry 的停滞清扫器兜底收尾——与 faces.go RunPipeline 对中断
// 的处理方式一致。
func (ix *Indexer) BackfillSprites(ctx context.Context) {
	if !ix.spriteBackfillRunning.CompareAndSwap(false, true) {
		return
	}
	defer ix.spriteBackfillRunning.Store(false)

	candidates, err := spriteBackfillCandidates(ix.db)
	if err != nil {
		zap.L().Warn("sprite 补跑候选查询失败", zap.Error(err))
		return
	}

	// 预扫描:逐条候选判 sprite.jpg / preview.mp4 是否缺失,只有任一缺失的才
	// 算一个待处理项,得到 total。两者都已齐备的候选不计入 total、也不进入
	// 下面的处理循环(本轮它无事可做)。total==0 直接返回,不发任务。
	var pending []spriteCandidate
	for _, c := range candidates {
		spritePath := filepath.Join(ix.thumbDir, c.id, "sprite.jpg")
		previewPath := filepath.Join(ix.thumbDir, c.id, "preview.mp4")
		_, spriteErr := os.Stat(spritePath)
		_, previewErr := os.Stat(previewPath)
		if spriteErr != nil || previewErr != nil {
			pending = append(pending, c)
		}
	}
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
	// scheduleRemove 是终态收尾:沿用 faces.go 的模式,done/error 后延迟
	// taskCleanupDelay 再从注册表摘除,给前端留出展示终态的窗口。
	scheduleRemove := func() {
		go func() {
			time.Sleep(taskCleanupDelay)
			if ix.taskReg != nil {
				ix.taskReg.Remove(taskID)
			}
		}()
	}

	pub(0, "running", "", nil)

	// sourceMissing 仅统计"源视频文件缺失"（os.Stat 失败）而整条候选被跳过的
	// 数量；sprite.jpg / preview.mp4 是否已存在则各自在下面独立 os.Stat 判断，
	// 不计入这个计数器（对应 spritesGenerated / previewsGenerated 未增长的隐含语义）。
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
					zap.L().Warn("ffmpeg 不可用,放弃本轮 sprite/preview 补跑", zap.Error(err))
					pub(current, "error", TaskErrPreviewFfmpegMissing, nil)
					scheduleRemove()
					return
				}
				zap.L().Warn("sprite 补跑失败", zap.String("asset_id", c.id), zap.Error(err))
				itemFailed = true
			} else {
				spritesGenerated++
			}
		}

		previewPath := filepath.Join(ix.thumbDir, c.id, "preview.mp4")
		if _, statErr := os.Stat(previewPath); statErr != nil {
			if err := ix.sprites.EnsurePreview(c.filePath, previewPath); err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					zap.L().Warn("ffmpeg 不可用,放弃本轮 sprite/preview 补跑", zap.Error(err))
					pub(current, "error", TaskErrPreviewFfmpegMissing, nil)
					scheduleRemove()
					return
				}
				zap.L().Warn("preview 补跑失败", zap.String("asset_id", c.id), zap.Error(err))
				itemFailed = true
			} else {
				previewsGenerated++
			}
		}

		if itemFailed {
			failed++
		}
		current++
		pub(current, "running", "", nil)
	}
	if spritesGenerated > 0 || previewsGenerated > 0 || failed > 0 {
		zap.L().Info("sprite/preview 补跑完成",
			zap.Int("sprites_generated", spritesGenerated),
			zap.Int("previews_generated", previewsGenerated),
			zap.Int("source_missing", sourceMissing),
			zap.Int64("failed", failed))
	}

	if failed > 0 && failed == total {
		// 对齐 BackfillOCR 惯例(backfillOCROnce):只有全部候选都失败才判整批
		// error;个别失败(哪怕反复出现,例如一条永久损坏的视频)只记日志,任务
		// 终态照常 done,避免任务栏每轮都弹 Failed 造成噪音。
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
