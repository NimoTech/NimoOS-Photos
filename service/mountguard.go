package service

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// removableRoots is the mount-point namespace MountGuard governs. Only
// /media/ is the automount namespace used for hot-plugged removable drives;
// /DATA and any manually-mounted volumes under /mnt (RAID arrays, MergerFS,
// NAS shares) are permanent storage and are never touched, even if they
// transiently vanish from /proc/mounts.
var removableRoots = []string{"/media/"}

// mountGuardPollInterval is how often MountGuard re-reads the mount table to
// detect a removable drive being unplugged or reinserted.
const mountGuardPollInterval = 30 * time.Second

// MountGuard keeps assets.offline in sync with whether the removable drive
// holding a given asset's file is currently mounted. Without this, unplugging
// a drive leaves its assets permanently "ghosted": fsnotify goes silent for
// paths under the vanished mount, ScanAllRoots only iterates the roots
// EnumerateScanRoots reports as *currently* mounted (so it never revisits the
// vanished one to notice its assets are gone), and the Embedder's backfill
// loop would otherwise burn ML calls retrying files it can never read. Flagging
// them offline instead lets every consumer skip them cleanly, and the flag
// self-heals automatically the moment the drive comes back.
type MountGuard struct {
	db       *sql.DB
	interval time.Duration

	// currentMounts returns the removable (/media/*) mount points that are
	// mounted right now. Defaults to currentRemovableMounts; overridden in
	// tests to avoid depending on the real /proc/mounts.
	currentMounts func() []string

	// Recovery hooks, run in this order after a removable mount reappears.
	// Injected as function fields (not concrete *Watcher/*Indexer/*Embedder
	// types) so MountGuard has no import-time dependency on them and stays
	// unit-testable in isolation.
	watcherRestart func()
	scanDir        func(mount string) error
	backfill       func(ctx context.Context) error
	backfillOCR    func(ctx context.Context) error

	mu         sync.Mutex
	lastMounts map[string]bool
}

// NewMountGuard creates a MountGuard backed by db. Recovery hooks default to
// no-ops; wire them with the Set* methods below before calling Run.
func NewMountGuard(db *sql.DB) *MountGuard {
	return &MountGuard{
		db:            db,
		interval:      mountGuardPollInterval,
		currentMounts: currentRemovableMounts,
	}
}

// SetPollInterval overrides the default 30s poll interval (tests only).
func (g *MountGuard) SetPollInterval(d time.Duration) { g.interval = d }

// SetWatcherRestart wires the callback that re-adds fsnotify watches after a
// removable mount reappears (typically services.RestartWatcher with the
// current configured WatchDirs — Add is a harmless no-op for directories the
// watcher isn't configured to watch).
func (g *MountGuard) SetWatcherRestart(f func()) { g.watcherRestart = f }

// SetScanDir wires the callback that re-scans a specific mount point after it
// reappears (typically Indexer.ScanDirectory), self-healing any file added or
// removed while the drive was offline.
func (g *MountGuard) SetScanDir(f func(mount string) error) { g.scanDir = f }

// SetBackfill wires the callback that repairs missing CLIP vectors after a
// mount reappears (typically Embedder.Backfill).
func (g *MountGuard) SetBackfill(f func(ctx context.Context) error) { g.backfill = f }

// SetBackfillOCR wires the callback that repairs missing OCR text after a
// mount reappears (typically Embedder.BackfillOCR).
func (g *MountGuard) SetBackfillOCR(f func(ctx context.Context) error) { g.backfillOCR = f }

// currentRemovableMounts is the production currentMounts implementation. It
// reuses EnumerateScanRoots' /proc/mounts parsing and keeps only the /media/*
// entries (removableRoots) — RAID arrays and other /mnt mounts are out of
// scope for offline tracking.
func currentRemovableMounts() []string {
	var out []string
	for _, m := range EnumerateScanRoots() {
		for _, prefix := range removableRoots {
			if strings.HasPrefix(m, prefix) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Run performs the startup alignment pass and then polls every interval
// until ctx is cancelled. Intended to be launched as `go mg.Run(ctx)` from
// main.go alongside the other background workers.
func (g *MountGuard) Run(ctx context.Context) {
	g.AlignOnStartup()

	g.mu.Lock()
	g.lastMounts = toMountSet(g.currentMounts())
	g.mu.Unlock()

	interval := g.interval
	if interval <= 0 {
		interval = mountGuardPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.checkOnce(ctx)
		}
	}
}

// checkOnce runs a single mount-set diff against the last known snapshot,
// marking assets offline/online and firing the recovery sequence for mounts
// that just reappeared.
func (g *MountGuard) checkOnce(ctx context.Context) {
	cur := toMountSet(g.currentMounts())

	g.mu.Lock()
	prev := g.lastMounts
	g.lastMounts = cur
	g.mu.Unlock()

	for m := range prev {
		if !cur[m] {
			g.markOffline(m)
		}
	}
	for m := range cur {
		if !prev[m] {
			g.markOnline(m)
			g.onMountBack(ctx, m)
		}
	}
}

// onMountBack runs the recovery sequence for a removable mount that just
// reappeared: re-add it to the fsnotify watcher, self-heal new/removed files
// with a scoped rescan, then repair any CLIP/OCR gap left by assets that were
// offline during a model-generation rebuild or an OCR backfill pass.
func (g *MountGuard) onMountBack(ctx context.Context, mount string) {
	if g.watcherRestart != nil {
		g.watcherRestart()
	}
	if g.scanDir != nil {
		if err := g.scanDir(mount); err != nil {
			zap.L().Warn("mountguard: rescan after remount failed",
				zap.String("mount", mount), zap.Error(err))
		}
	}
	if g.backfill != nil {
		if err := g.backfill(ctx); err != nil {
			zap.L().Warn("mountguard: CLIP backfill after remount failed",
				zap.String("mount", mount), zap.Error(err))
		}
	}
	if g.backfillOCR != nil {
		if err := g.backfillOCR(ctx); err != nil {
			zap.L().Warn("mountguard: OCR backfill after remount failed",
				zap.String("mount", mount), zap.Error(err))
		}
	}
}

// AlignOnStartup reconciles assets.offline against the current mount table
// once at startup, catching drives that were unplugged (or replugged) while
// the service was down — the periodic poll only observes transitions that
// happen while it's running.
func (g *MountGuard) AlignOnStartup() {
	cur := toMountSet(g.currentMounts())
	for prefix := range g.assetMountCandidates() {
		if cur[prefix] {
			g.markOnline(prefix)
		} else {
			g.markOffline(prefix)
		}
	}
}

// assetMountCandidates derives the set of removable mount-point prefixes
// referenced by any asset currently in the library, by taking the first
// three path segments of every distinct /media/* file_path (e.g.
// "/media/devmon/sdg1-usb-Kingston_DataTra/DCIM/x.jpg" yields the mount point
// "/media/devmon/sdg1-usb-Kingston_DataTra"). That's how devmon/udevil name
// automount points, matching what EnumerateScanRoots/currentRemovableMounts
// report as a mount point.
func (g *MountGuard) assetMountCandidates() map[string]bool {
	out := map[string]bool{}
	rows, err := g.db.Query(`SELECT DISTINCT file_path FROM assets WHERE file_path LIKE '/media/%'`)
	if err != nil {
		zap.L().Warn("mountguard: query asset mount candidates failed", zap.Error(err))
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil {
			continue
		}
		if prefix, ok := mountPrefixFromAssetPath(p); ok {
			out[prefix] = true
		}
	}
	return out
}

// mountPrefixFromAssetPath extracts the leading 3-segment mount-point prefix
// (e.g. "/media/devmon/sdg1-usb-Kingston_DataTra") from an asset file_path
// under /media/. Returns ok=false if the path has fewer than 3 segments.
func mountPrefixFromAssetPath(p string) (string, bool) {
	trimmed := strings.TrimPrefix(p, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", false
	}
	return "/" + strings.Join(parts[:3], "/"), true
}

// markOffline flags every currently-online asset under mount as offline.
func (g *MountGuard) markOffline(mount string) {
	res, err := g.db.Exec(`UPDATE assets SET offline=1 WHERE offline=0 AND file_path LIKE ?`, mount+"/%")
	if err != nil {
		zap.L().Warn("mountguard: mark offline failed", zap.String("mount", mount), zap.Error(err))
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		zap.L().Info("mountguard: marked assets offline (drive unplugged)",
			zap.String("mount", mount), zap.Int64("count", n))
	}
}

// markOnline flags every offline asset under mount back online.
func (g *MountGuard) markOnline(mount string) {
	res, err := g.db.Exec(`UPDATE assets SET offline=0 WHERE offline=1 AND file_path LIKE ?`, mount+"/%")
	if err != nil {
		zap.L().Warn("mountguard: mark online failed", zap.String("mount", mount), zap.Error(err))
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		zap.L().Info("mountguard: marked assets online (drive reinserted)",
			zap.String("mount", mount), zap.Int64("count", n))
	}
}

func toMountSet(mounts []string) map[string]bool {
	set := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		set[m] = true
	}
	return set
}
