package service

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// overflowRescanCooldown 限制两次溢出补扫之间的最小间隔:写入风暴期间
// inotify 队列会连环溢出,单飞(overflowRescanning)只防止并发,不防止
// 补扫刚结束、下一次溢出又立刻触发——每次全树补扫都是一次 IO 开销,风暴期
// 内的连环溢出应合并进冷却期内已完成/正在进行的这一轮,交给周期性
// ScanDirectory 兜底覆盖剩余未扫到的变更。
const overflowRescanCooldown = 5 * time.Minute

// watchPollInterval 是自动模式下根集合轮询的默认间隔,与
// mountguard.go 的 mountGuardPollInterval 同频。
const watchPollInterval = 30 * time.Second

// Watcher monitors directories for new or modified media files and enqueues
// them for indexing. It also provides live-photo pairing via PairLivePhotos.
//
// inotify (the only backend fsnotify uses on Linux) is NOT recursive: adding a
// watch on a directory only reports events for that directory's immediate
// children, never grandchildren. Watcher therefore walks each configured
// WatchDir at startup and adds a watch on every eligible subdirectory
// (addRecursiveWatch), and tracks newly created directories as they appear
// (trackNewDir) so files dropped several levels deep are never silently
// missed until the next full ScanAllRoots (up to 24h later).
type Watcher struct {
	db        *sql.DB
	watchDirs []string
	indexer   *Indexer
	liveDir   string
	cancel    context.CancelFunc
	mu        sync.Mutex

	// roots holds the resolved watch roots as of the last Start (see
	// resolveWatchDirRoot) — the same paths addRecursiveWatch was called
	// with. handleWatchError re-walks these on an inotify queue overflow.
	roots []string
	// overflowRescanning single-flights the overflow recovery rescan:
	// overflow errors tend to arrive in bursts, and only one rescan needs
	// to be in flight at a time (walkSupported + Indexer.Enqueue are
	// idempotent, so a rescan already covers anything a second one would).
	overflowRescanning atomic.Bool
	// lastOverflowRescan 记录上一轮溢出补扫结束的 Unix 秒时间戳,配合
	// overflowRescanCooldown 抑制写入风暴下的连环补扫。
	lastOverflowRescan atomic.Int64
	// walkInFlight tracks directories (keyed by withSep) whose trackNewDir
	// recursive walk is currently running. A dense mkdir burst (e.g. an
	// entire subtree being copied/moved in) can fire a Create event for
	// every intermediate directory; without dedup each one spawns its own
	// full recursive walk of the same subtree via addRecursiveWatch +
	// walkSupported, multiplying IO for no benefit — the outermost walk
	// already covers everything beneath it.
	walkInFlight sync.Map

	// enumerateRoots 是自动模式(watchDirs 为空)下的根集合来源;nil 时用
	// EnumerateScanRoots(生产路径),测试注入以避免依赖真实 /proc/mounts。
	enumerateRoots func() []string

	// pollInterval 是自动模式下根集合轮询间隔;0 ⇒ watchPollInterval(30s,
	// 与 mountGuardPollInterval 同频)。测试注入短间隔。
	pollInterval time.Duration
}

// NewWatcher creates a new Watcher.
func NewWatcher(db *sql.DB, watchDirs []string, indexer *Indexer, liveDir string) *Watcher {
	return &Watcher{
		db:        db,
		watchDirs: watchDirs,
		indexer:   indexer,
		liveDir:   liveDir,
	}
}

// Start begins watching all configured directories (and, recursively, every
// eligible subdirectory beneath them). Directories that cannot be watched are
// logged as warnings but do not abort startup. The function blocks until the
// internal context (derived from parentCtx) is cancelled. Calling Restart
// cancels the previous Start goroutine and spawns a new one.
func (w *Watcher) Start(parentCtx context.Context) {
	w.mu.Lock()
	ctx, cancel := context.WithCancel(parentCtx)
	if w.cancel != nil {
		w.cancel()
	}
	w.cancel = cancel
	dirs := append([]string(nil), w.watchDirs...)
	enumerateRoots := w.enumerateRoots
	w.mu.Unlock()

	auto := len(dirs) == 0
	if auto {
		enum := enumerateRoots
		if enum == nil {
			enum = EnumerateScanRoots
		}
		dirs = enum()
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		zap.L().Error("watcher: failed to create fsnotify watcher", zap.Error(err))
		return
	}
	// wg tracks goroutines spawned for newly-discovered directories
	// (trackNewDir). It must be waited on BEFORE fw is closed, so a pending
	// fw.Add / catch-up scan never races a closed watcher — hence the
	// deferred wg.Wait() is registered after defer fw.Close(), making it run
	// first (LIFO).
	var wg sync.WaitGroup
	defer fw.Close()
	defer wg.Wait()

	totalWatches := 0
	resolvedRoots := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		root, ok := resolveWatchDirRoot(dir)
		if !ok {
			continue
		}
		resolvedRoots = append(resolvedRoots, root)
		added, _ := addRecursiveWatch(ctx, fw, root)
		totalWatches += added
	}
	w.mu.Lock()
	w.roots = resolvedRoots
	w.mu.Unlock()
	zap.L().Info("watcher: started",
		zap.Strings("watchDirs", dirs), zap.Bool("auto", auto), zap.Int("watches", totalWatches))

	if auto {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.followMounts(ctx, parentCtx, dirs)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fw.Events:
			if !ok {
				return
			}
			w.handleEvent(ctx, fw, &wg, event)
		case watchErr, ok := <-fw.Errors:
			if !ok {
				return
			}
			w.handleWatchError(ctx, &wg, watchErr)
		}
	}
}

// handleWatchError:inotify 队列溢出意味着事件已经丢失,唯一可靠的恢复
// 是对所有根做一次补扫(Enqueue 幂等,重复入队无害)。单飞防抖:溢出
// 往往连环出现,补扫进行中忽略后续溢出。
func (w *Watcher) handleWatchError(ctx context.Context, wg *sync.WaitGroup, err error) {
	if !errors.Is(err, fsnotify.ErrEventOverflow) {
		zap.L().Warn("watcher: fsnotify error", zap.Error(err))
		return
	}
	if time.Now().Unix()-w.lastOverflowRescan.Load() < int64(overflowRescanCooldown/time.Second) {
		return // 风暴期内的连环溢出合并进上一轮补扫,交给周期扫描兜底
	}
	if !w.overflowRescanning.CompareAndSwap(false, true) {
		return
	}
	zap.L().Warn("watcher: event queue overflow — starting recovery rescan")
	w.mu.Lock()
	roots := append([]string(nil), w.roots...)
	w.mu.Unlock()
	// Tracked by wg (like trackNewDir's goroutines) so Start() cannot return
	// and close fw while this rescan is still running.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer w.overflowRescanning.Store(false)
		defer w.lastOverflowRescan.Store(time.Now().Unix())
		for _, root := range roots {
			if err := walkSupported(ctx, root, func(p string) { w.indexer.Enqueue(p) }); err != nil && !errors.Is(err, context.Canceled) {
				zap.L().Warn("watcher: overflow rescan failed", zap.String("root", root), zap.Error(err))
			}
		}
	}()
}

// handleEvent processes a single fsnotify event. Create is the only event
// type that needs a filesystem Stat (to tell a newly-appeared directory apart
// from a newly-appeared file), so the Stat call is made at most once, only
// for Create events — Write/Remove/Rename never need it.
func (w *Watcher) handleEvent(ctx context.Context, fw *fsnotify.Watcher, wg *sync.WaitGroup, event fsnotify.Event) {
	if event.Has(fsnotify.Create) {
		if fi, statErr := os.Stat(event.Name); statErr == nil && fi.IsDir() {
			// A new directory appeared — either a plain mkdir (files may
			// follow shortly after) or an entire subtree moved/copied in
			// atomically (files already present). Track it in its own
			// goroutine so a large tree doesn't stall the event loop; the
			// goroutine is tracked by wg so Start() cannot return (and close
			// fw) while it is still running.
			wg.Add(1)
			go func(dir string) {
				defer wg.Done()
				w.trackNewDir(ctx, fw, dir)
			}(event.Name)
		} else if isSupportedMedia(event.Name) {
			w.indexer.Enqueue(event.Name)
		}
	} else if event.Has(fsnotify.Write) && isSupportedMedia(event.Name) {
		w.indexer.Enqueue(event.Name)
	}

	// Directory deletes/renames: inotify automatically drops the watch
	// (IN_IGNORED) when a watched directory is deleted or moved away, and
	// fsnotify's internal watch-descriptor bookkeeping follows suit — there is
	// nothing to clean up on that front. But the DB index still needs
	// explicit cleanup: fsnotify reports the removal with event.Name set to
	// the directory's own path — not one event per file inside it — so a
	// directory-shaped path (no recognised media extension) is routed through
	// shouldHandleDeletedPath + pruneMissingUnder (service/busdelete.go,
	// service/indexer.go), the same "no extension ⇒ possibly a directory"
	// handling the MessageBus deletion subscriber uses. Without this, every
	// asset indexed from under the deleted directory — CLIP vector and
	// thumbnail included — would linger until the next 24h ScanAllRoots.
	// For files, RemoveByPath still runs as before (exact-path fast path).
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		if isSupportedMedia(event.Name) {
			w.indexer.RemoveByPath(event.Name)
		} else if shouldHandleDeletedPath(event.Name) {
			// pruneMissingUnder is called with the PARENT of the deleted
			// directory, not the directory itself. Its own safety interlock
			// (pruneDeleteAllowed, service/indexer.go) re-validates right
			// before deleting by os.Stat'ing the "dir" it was given — the
			// exact directory this event just reported gone will always fail
			// that stat, since it no longer exists, and pruneMissingUnder
			// would silently no-op every single time. The parent, in
			// contrast, is guaranteed to still be on disk: directories are
			// only ever removable once empty, so an rm -rf (or any other
			// recursive delete) always rmdir's bottom-up — a directory's own
			// Remove/Rename event can only fire after everything beneath it
			// is already gone, and strictly before its parent is touched.
			// Scanning from the parent still only deletes rows that
			// individually stat as missing (siblings that still exist are
			// left untouched), it just widens the query/stat pass to the
			// parent's subtree instead of only the deleted directory's.
			wg.Add(1)
			go func(dir string) {
				defer wg.Done()
				if err := w.indexer.pruneMissingUnder(filepath.Dir(dir)); err != nil {
					zap.L().Warn("watcher: prune after directory delete failed",
						zap.String("dir", dir), zap.Error(err))
				}
			}(event.Name)
		}
	}
}

// trackNewDir is invoked (in its own goroutine, tracked by Start's WaitGroup)
// whenever a Create event's target turns out to be a directory. It adds the
// new directory — and any subdirectories it may already contain — to fw via
// addRecursiveWatch, then performs a one-time catch-up scan enqueuing any
// supported media files already present. The catch-up scan is what closes the
// two windows a bare "add a watch" cannot: "mkdir now, files land a moment
// later" (files created before the watch takes effect) and "an entire
// subtree — directories and files — moved/copied in as one atomic rename"
// (nothing under it was ever watched at all until this goroutine runs).
func (w *Watcher) trackNewDir(ctx context.Context, fw *fsnotify.Watcher, dir string) {
	// A dynamically discovered directory must NOT inherit addRecursiveWatch's
	// root exemption from the hidden-dir check: that exemption exists solely
	// so an explicitly configured WatchDir is honoured as-is. A hidden
	// directory created at runtime (e.g. TrashService making .trash/<id>/
	// under /DATA/Gallery on the first soft-delete) must stay invisible —
	// watching it would leak one inotify watch per deletion, re-enqueue every
	// trashed file (wasted read + SHA-256), and violate walkSupported's
	// "soft-deleted files are never re-indexed" invariant (indexer.go).
	if strings.HasPrefix(filepath.Base(dir), ".") {
		return
	}
	key := withSep(dir)
	// ancestorCovered must be evaluated BEFORE this directory registers its
	// own key below — walkCovered does a prefix match, so checking it any
	// later would trivially match dir against itself (every string is a
	// prefix of itself) and wrongly report "covered" on every single call,
	// permanently disabling the catch-up scan. Evaluated here, it only
	// reflects whether a genuine ANCESTOR walk is already in flight.
	ancestorCovered := w.walkCovered(dir)
	w.walkInFlight.Store(key, struct{}{})
	defer w.walkInFlight.Delete(key)

	// addRecursiveWatch always runs, even when ancestorCovered is true:
	// filepath.WalkDir is a single pre-order pass, so an ancestor's
	// in-flight walk may already have snapshotted this directory's parent
	// before `dir` existed — meaning the ancestor's walk will never revisit
	// it. Skipping the watch here (as a prior version of this function did)
	// left such directories permanently unwatched: any file dropped into
	// them afterward would go undetected until the next full 24h rescan.
	// fw.Add is cheap and idempotent, so calling it unconditionally is safe.
	added, enospc := addRecursiveWatch(ctx, fw, dir)
	if skipCatchupScan(added, enospc) {
		// 目录被排除(scanExcludeDirs/IsExcludedMount)或 walk 前已消失:
		// 没有 watch 覆盖也没有内容需要索引。
		return
	}
	if enospc && added == 0 {
		zap.L().Warn("watcher: no watches added (inotify quota) — indexing catch-up still runs, future changes untracked until quota raised",
			zap.String("dir", dir))
	}
	zap.L().Info("watcher: now watching new directory",
		zap.String("dir", dir), zap.Int("watches", added))

	// Dedup only applies to the redundant catch-up INDEX scan below: if an
	// ancestor's walk is already in flight, it will walk this subtree too
	// and enqueue everything under it, so re-walking here for indexing is
	// wasted IO. Watch coverage above is never skipped by this dedup.
	if ancestorCovered {
		return
	}
	// walkSupported (service/indexer.go) already encodes the same
	// hidden/scanExcludeDirs skip rules used by the full scanner, so the
	// catch-up scan can't index anything the periodic scan would have
	// skipped either.
	if err := walkSupported(ctx, dir, func(path string) {
		w.indexer.Enqueue(path)
	}); err != nil && !errors.Is(err, context.Canceled) {
		zap.L().Warn("watcher: catch-up scan failed", zap.String("dir", dir), zap.Error(err))
	}
}

// withSep 规整目录键,保证前缀判断不会把 /a/bc 误判为 /a/b 的子目录
func withSep(dir string) string {
	return strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator)
}

// walkCovered reports whether dir itself or any ancestor already has a
// recursive walk in flight(祖先的 WalkDir 必然覆盖 dir,重复走只是放大 IO)。
func (w *Watcher) walkCovered(dir string) bool {
	key := withSep(dir)
	covered := false
	w.walkInFlight.Range(func(k, _ any) bool {
		if strings.HasPrefix(key, k.(string)) {
			covered = true
			return false
		}
		return true
	})
	return covered
}

// addRecursiveWatch walks root and adds an inotify watch on root itself and
// every eligible subdirectory beneath it, stopping early if ctx is cancelled.
// It skips (via fs.SkipDir):
//   - any directory literally named ".snapshots", unconditionally — including
//     root itself (see isInSnapshotsDir, service/snapshots.go). btrbk/snapper
//     mount each read-only hourly btrfs snapshot subvolume as its own
//     /proc/mounts entry under "<volume mountpoint>/.snapshots/<ts>/", so root
//     here can itself already be nested inside .snapshots without its own
//     basename being ".snapshots" — the IsExcludedMount(path) check below
//     already catches that case too (isInSnapshotsDir is folded into it), this
//     check is the explicit, no-lookup-required first line of defense;
//   - hidden directories (basename starting with ".") — except root itself,
//     mirroring walkSupported's convention so an explicitly configured
//     WatchDir is never silently ignored;
//   - scanExcludeDirs (service/indexer.go), including root itself — these
//     hold app/system data, never user media;
//   - any path IsExcludedMount reports as an excluded mount (known OS system
//     mount, a devmon removable-media mount, or a ".snapshots"-nested path),
//     including root itself — this closes the gap where an admin manually
//     configuring, say, /media/devmon/USB1 as a WatchDir would otherwise get
//     it watched even though the rest of the codebase treats that mount as
//     off-limits.
//
// addRecursiveWatch never follows symlinks: filepath.WalkDir uses lstat
// semantics throughout, so a symlink — whether it is root itself or appears
// anywhere in the tree — contributes zero watches. This is deliberate and
// mirrors walkSupported (the periodic scan): resolving symlinks here would
// let a runtime symlink created inside a watched tree (Create event →
// trackNewDir → this function) pull its ENTIRE external target tree into
// watching and indexing — a symlink to / would watch nearly the whole
// filesystem, exhaust the inotify quota, and index out-of-library content
// the scan would never touch. Explicitly configured WatchDirs that are
// symlinks are the one sanctioned exception, resolved by the caller in
// Start via resolveWatchDirRoot BEFORE this function runs.
//
// A single directory failing to be added (permission error, or the inotify
// watch quota being exhausted) is logged as a warning and does not abort the
// walk — sibling and descendant directories are still attempted. Returns the
// number of directories successfully added, and whether any fw.Add call hit
// ENOSPC (inotify watch quota exhausted). Callers must not treat added==0
// alone as "nothing to do here" — see skipCatchupScan.
func addRecursiveWatch(ctx context.Context, fw *fsnotify.Watcher, root string) (int, bool) {
	added := 0
	enospc := false
	enospcWarned := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			zap.L().Warn("watcher: walk error", zap.String("path", path), zap.Error(err))
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == snapshotsDirName {
			return filepath.SkipDir
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if scanExcludeDirs[path] {
			return filepath.SkipDir
		}
		if IsExcludedMount(path) {
			zap.L().Warn("watcher: refusing to watch excluded mount",
				zap.String("path", path))
			return filepath.SkipDir
		}
		if addErr := fw.Add(path); addErr != nil {
			if errors.Is(addErr, syscall.ENOSPC) {
				enospc = true
				if !enospcWarned {
					zap.L().Warn("watcher: inotify watch limit reached — raise fs.inotify.max_user_watches",
						zap.String("dir", path), zap.Error(addErr))
					enospcWarned = true
				}
			} else {
				zap.L().Warn("watcher: failed to watch directory",
					zap.String("dir", path), zap.Error(addErr))
			}
			return nil
		}
		added++
		return nil
	})
	return added, enospc
}

// skipCatchupScan decides whether trackNewDir should skip its one-time
// catch-up scan after addRecursiveWatch. added==0 is ambiguous by itself: it
// means either "no watch coverage was ever intended" (directory excluded via
// scanExcludeDirs/IsExcludedMount, or it vanished before the walk ran — safe
// to skip, there is nothing to index) or "every fw.Add call hit ENOSPC"
// (inotify watch quota exhausted, but the directory and its files are real
// and must still be indexed — only future-change tracking degrades until the
// quota is raised). Only the former should skip the scan.
func skipCatchupScan(added int, enospc bool) bool {
	return added == 0 && !enospc
}

// resolveWatchDirRoot resolves an explicitly configured WatchDir for use as
// an addRecursiveWatch root. Only configured roots get symlink resolution:
// the old non-recursive fw.Add followed symlinks (inotify_add_watch resolves
// them), so a WatchDir configured as a symlink to a real directory must keep
// working — but filepath.WalkDir lstat's its root and would otherwise
// silently yield zero watches. Dynamically discovered directories
// (trackNewDir) must NOT receive this treatment; they keep WalkDir's lstat
// semantics so a runtime symlink can never pull an external tree into
// watching (see addRecursiveWatch's doc comment).
//
// Returns (resolvedPath, true) on success — events and Enqueue'd paths then
// carry the resolved path, matching what the periodic scan indexes — or
// ("", false) when dir is a symlink that cannot be resolved (dangling, loop),
// which is logged and skipped.
func resolveWatchDirRoot(dir string) (string, bool) {
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		// Missing paths fall through unchanged: addRecursiveWatch's walk will
		// log the error the same way any unreadable WatchDir is reported.
		return dir, true
	}
	resolved, rerr := filepath.EvalSymlinks(dir)
	if rerr != nil {
		zap.L().Warn("watcher: cannot resolve symlinked watch dir",
			zap.String("dir", dir), zap.Error(rerr))
		return "", false
	}
	return resolved, true
}

// Restart updates the watched directories and restarts the watcher goroutine.
func (w *Watcher) Restart(parentCtx context.Context, dirs []string) {
	w.mu.Lock()
	w.watchDirs = dirs
	w.mu.Unlock()
	go w.Start(parentCtx)
}

// followMounts 是自动模式的动态跟随:周期性比较根集合快照,发现挂载增减就
// 触发 Restart 重建监听;新出现的根先补扫一遍(inotify 只看未来事件,存量
// 文件靠补扫入库,ScanDirectoryOnce 与 MountGuard 恢复补扫天然去重)。
// 触发重启后本轮询即退出——Restart 会启动新的 Start,新 Start 自带新轮询。
func (w *Watcher) followMounts(ctx context.Context, parentCtx context.Context, current []string) {
	interval := w.pollInterval
	if interval <= 0 {
		interval = watchPollInterval
	}
	enum := w.enumerateRoots
	if enum == nil {
		enum = EnumerateScanRoots
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := enum()
			added := diffNewRoots(current, next)
			removed := diffNewRoots(next, current)
			if len(added) == 0 && len(removed) == 0 {
				continue
			}
			zap.L().Info("watcher: mount set changed, restarting",
				zap.Strings("added", added), zap.Strings("removed", removed))
			for _, root := range added {
				go func(dir string) {
					if _, err := w.indexer.ScanDirectoryOnce(dir); err != nil {
						zap.L().Warn("watcher: catch-up scan failed",
							zap.String("dir", dir), zap.Error(err))
					}
				}(root)
			}
			w.Restart(parentCtx, nil) // nil 保持自动模式
			return
		}
	}
}

// diffNewRoots returns the elements of next that are not in old(顺序无关)。
func diffNewRoots(old, next []string) []string {
	seen := make(map[string]bool, len(old))
	for _, r := range old {
		seen[r] = true
	}
	var out []string
	for _, r := range next {
		if !seen[r] {
			out = append(out, r)
		}
	}
	return out
}

// PairLivePhotos scans all un-paired MOV files and attempts to match them with
// a still image (JPEG or HEIC) sharing the same base name. When a pair is
// found, the still's live_photo_video_id is set to the MOV asset ID, and the
// MOV is flagged as is_live_photo_video=1.
func (w *Watcher) PairLivePhotos() error {
	rows, err := w.db.Query(`
		SELECT id, file_path
		FROM assets
		WHERE is_live_photo_video = 0
		  AND live_photo_video_id IS NULL
		  AND (mime_type LIKE 'video/%'
		       OR LOWER(SUBSTR(file_path, LENGTH(file_path) - 3)) = '.mov')
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	stillExts := []string{".jpg", ".jpeg", ".heic", ".JPG", ".JPEG", ".HEIC"}

	for rows.Next() {
		var movID, movPath string
		if err := rows.Scan(&movID, &movPath); err != nil {
			continue
		}

		ext := filepath.Ext(movPath)
		base := strings.TrimSuffix(movPath, ext)

		var stillID string
		for _, se := range stillExts {
			candidate := base + se
			queryErr := w.db.QueryRow(
				`SELECT id FROM assets WHERE file_path = ?`, candidate,
			).Scan(&stillID)
			if queryErr == nil {
				break
			}
		}

		if stillID == "" {
			continue
		}

		// Link the still to the MOV.
		if _, err := w.db.Exec(
			`UPDATE assets SET live_photo_video_id = ? WHERE id = ?`,
			movID, stillID,
		); err != nil {
			zap.L().Warn("watcher: failed to set live_photo_video_id",
				zap.String("stillID", stillID), zap.Error(err))
			continue
		}

		// Mark the MOV as a live-photo video.
		if _, err := w.db.Exec(
			`UPDATE assets SET is_live_photo_video = 1 WHERE id = ?`,
			movID,
		); err != nil {
			zap.L().Warn("watcher: failed to set is_live_photo_video",
				zap.String("movID", movID), zap.Error(err))
		}
	}

	return rows.Err()
}

// isSupportedMedia reports whether path has a supported media file extension.
// It shares the single supportedExts table defined in indexer.go so the watcher
// and the indexer can never drift out of sync.
func isSupportedMedia(path string) bool {
	return supportedExts[strings.ToLower(filepath.Ext(path))]
}
