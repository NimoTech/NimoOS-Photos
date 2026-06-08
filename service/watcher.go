package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// Watcher monitors directories for new or modified media files and enqueues
// them for indexing. It also provides live-photo pairing via PairLivePhotos.
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

// Start begins watching all configured directories. Directories that cannot be
// watched are logged as warnings but do not abort startup. The function blocks
// until the internal context (derived from parentCtx) is cancelled. Calling
// Restart cancels the previous Start goroutine and spawns a new one.
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
	defer fw.Close()

	for _, dir := range dirs {
		if addErr := fw.Add(dir); addErr != nil {
			zap.L().Warn("watcher: failed to watch directory",
				zap.String("dir", dir), zap.Error(addErr))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fw.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				if isSupportedMedia(event.Name) {
					w.indexer.Enqueue(event.Name)
				}
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				if isSupportedMedia(event.Name) {
					w.indexer.RemoveByPath(event.Name)
				}
			}
		case watchErr, ok := <-fw.Errors:
			if !ok {
				return
			}
			zap.L().Warn("watcher: fsnotify error", zap.Error(watchErr))
		}
	}
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
