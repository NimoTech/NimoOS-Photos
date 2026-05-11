package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

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
// until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		zap.L().Error("watcher: failed to create fsnotify watcher", zap.Error(err))
		return
	}
	defer fw.Close()

	for _, dir := range w.watchDirs {
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
		case watchErr, ok := <-fw.Errors:
			if !ok {
				return
			}
			zap.L().Warn("watcher: fsnotify error", zap.Error(watchErr))
		}
	}
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
func isSupportedMedia(path string) bool {
	supported := map[string]bool{
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
	return supported[strings.ToLower(filepath.Ext(path))]
}
