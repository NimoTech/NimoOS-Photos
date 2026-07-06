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
	"syscall"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

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
	w.mu.Unlock()

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
	for _, dir := range dirs {
		root, ok := resolveWatchDirRoot(dir)
		if !ok {
			continue
		}
		totalWatches += addRecursiveWatch(ctx, fw, root)
	}
	zap.L().Info("watcher: started",
		zap.Strings("watchDirs", dirs), zap.Int("watches", totalWatches))

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
			zap.L().Warn("watcher: fsnotify error", zap.Error(watchErr))
		}
	}
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

	// Directory deletes/renames need no explicit handling here: inotify
	// automatically drops the watch (IN_IGNORED) when a watched directory is
	// deleted or moved away, and fsnotify's internal watch-descriptor
	// bookkeeping follows suit — there is nothing for Watcher to clean up.
	// For files, RemoveByPath still needs to run as before.
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		if isSupportedMedia(event.Name) {
			w.indexer.RemoveByPath(event.Name)
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
	added := addRecursiveWatch(ctx, fw, dir)
	if added == 0 {
		// Nothing was watched: the directory is excluded (scanExcludeDirs /
		// IsExcludedMount), vanished before the walk ran, or every fw.Add
		// failed (permissions, inotify quota). Either way there is no watch
		// coverage, so skip the catch-up scan too — it would only index files
		// whose future changes we then cannot track.
		return
	}
	zap.L().Info("watcher: now watching new directory",
		zap.String("dir", dir), zap.Int("watches", added))

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

// addRecursiveWatch walks root and adds an inotify watch on root itself and
// every eligible subdirectory beneath it, stopping early if ctx is cancelled.
// It skips (via fs.SkipDir):
//   - hidden directories (basename starting with ".") — except root itself,
//     mirroring walkSupported's convention so an explicitly configured
//     WatchDir is never silently ignored;
//   - scanExcludeDirs (service/indexer.go), including root itself — these
//     hold app/system data, never user media;
//   - any path IsExcludedMount reports as an excluded mount (known OS system
//     mount, or a devmon removable-media mount), including root itself — this
//     closes the gap where an admin manually configuring, say,
//     /media/devmon/USB1 as a WatchDir would otherwise get it watched even
//     though the rest of the codebase treats that mount as off-limits.
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
// number of directories successfully added.
func addRecursiveWatch(ctx context.Context, fw *fsnotify.Watcher, root string) int {
	added := 0
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
	return added
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
