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
// /DATA and any manually-mounted volumes under /mnt (MergerFS, NAS shares)
// are permanent storage and are never touched, even if they transiently
// vanish from /proc/mounts.
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
	recovering map[string]bool // per-mount in-flight recovery dedup
}

// NewMountGuard creates a MountGuard backed by db. Recovery hooks default to
// no-ops; wire them with the Set* methods below before calling Run.
func NewMountGuard(db *sql.DB) *MountGuard {
	return &MountGuard{
		db:            db,
		interval:      mountGuardPollInterval,
		currentMounts: currentRemovableMounts,
		recovering:    map[string]bool{},
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
// entries (removableRoots) — /DATA and /mnt mounts are out of scope for
// offline tracking.
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
// main.go alongside the other background workers. The startup alignment and
// the initial poll snapshot deliberately share one mount-table read, so a
// drive that (dis)appears between two separate reads can't be missed or
// double-handled.
func (g *MountGuard) Run(ctx context.Context) {
	cur := toMountSet(g.currentMounts())
	g.alignWith(cur)

	g.mu.Lock()
	g.lastMounts = cur
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

// AlignOnStartup reconciles assets.offline against the current mount table
// once, with a fresh mount enumeration. Exposed for tests; Run performs the
// same alignment inline, sharing one enumeration with its snapshot seeding.
func (g *MountGuard) AlignOnStartup() {
	g.alignWith(toMountSet(g.currentMounts()))
}

// alignWith reconciles assets.offline against the given mount set, catching
// drives that were unplugged (or replugged) while the service was down — the
// periodic poll only observes transitions that happen while it's running.
//
// Both directions are driven by the mount set itself; a mount point is NEVER
// guessed from an asset path (mount points can be any depth: /media/RAID_0 is
// 2 segments, devmon's /media/devmon/<label> is 3, so no fixed segment count
// is correct):
//   - restore: every asset under a currently-present mount goes online;
//   - mark:    every /media/* asset NOT under ANY currently-present mount
//     goes offline (with nested mounts, matching any one mount counts as
//     online).
//
// The restore direction here only fixes the flag; it deliberately does NOT
// fire the onMountBack recovery hooks. Drives that came back while the
// service was down are already covered by the normal startup path:
// NewService's initial ScanAllRoots re-scans every currently-mounted root
// (healing adds/removes), and the Embedder's ML ready transition (false→true
// on its first successful poll) triggers Backfill/BackfillOCR for any missing
// CLIP/OCR data.
func (g *MountGuard) alignWith(cur map[string]bool) {
	for m := range cur {
		_ = g.markOnline(m)
	}
	g.markOfflineOutside(cur)
}

// markOfflineOutside flags offline every online /media/* asset whose path is
// not under any mount in cur. Prefix matching uses substr(), not LIKE — mount
// names routinely contain LIKE metacharacters (e.g. a USB stick labelled
// Kingston_DataTra: `_` matches any character in a LIKE pattern and would
// bleed onto sibling mounts).
func (g *MountGuard) markOfflineOutside(cur map[string]bool) {
	q := `UPDATE assets SET offline=1 WHERE offline=0 AND substr(file_path,1,7)='/media/'`
	var args []any
	for m := range cur {
		q += ` AND NOT substr(file_path,1,length(?))=?`
		p := strings.TrimRight(m, "/") + "/"
		args = append(args, p, p)
	}
	res, err := g.db.Exec(q, args...)
	if err != nil {
		zap.L().Warn("mountguard: startup offline alignment failed", zap.Error(err))
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		zap.L().Info("mountguard: marked assets offline at startup (drive absent)",
			zap.Int64("count", n))
	}
}

// checkOnce runs a single mount-set diff against the last known snapshot,
// marking assets offline/online and firing the recovery sequence for mounts
// that just reappeared. If an UPDATE fails, the affected mount keeps its old
// snapshot state so the missed transition is retried on the next tick instead
// of being lost forever.
func (g *MountGuard) checkOnce(ctx context.Context) {
	cur := toMountSet(g.currentMounts())

	g.mu.Lock()
	prev := g.lastMounts
	g.mu.Unlock()

	next := make(map[string]bool, len(cur))
	for m := range cur {
		next[m] = true
	}

	for m := range prev {
		if !cur[m] {
			if err := g.markOffline(m); err != nil {
				next[m] = true // keep in snapshot: retry the unplug transition next tick
			}
		}
	}
	for m := range cur {
		if !prev[m] {
			if err := g.markOnline(m); err != nil {
				delete(next, m) // drop from snapshot: retry the replug transition next tick
				continue
			}
			g.spawnRecovery(ctx, m)
		}
	}

	g.mu.Lock()
	g.lastMounts = next
	g.mu.Unlock()
}

// spawnRecovery runs onMountBack in its own goroutine so a long recovery
// (ScanDirectory is synchronous and ML-bound — potentially hours on a large
// drive) never blocks the 30s poll loop from noticing OTHER drives being
// unplugged meanwhile. A per-mount in-flight flag dedupes re-triggers: if the
// same mount bounces (unplug+replug) while its recovery is still running, the
// second trigger is dropped instead of stacking a concurrent duplicate scan.
func (g *MountGuard) spawnRecovery(ctx context.Context, mount string) {
	g.mu.Lock()
	if g.recovering[mount] {
		g.mu.Unlock()
		return
	}
	g.recovering[mount] = true
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.recovering, mount)
			g.mu.Unlock()
		}()
		g.onMountBack(ctx, mount)
	}()
}

// onMountBack runs the recovery sequence for a removable mount that just
// reappeared: re-add it to the fsnotify watcher, self-heal new/removed files
// with a scoped rescan, then repair any CLIP/OCR gap left by assets that were
// offline during a model-generation rebuild or an OCR backfill pass.
//
// Known limitation (accepted): the scoped rescan may overlap with a
// concurrently running ScanAllRoots tick or a manual rebuild touching the
// same files. That only wastes some duplicate hashing/ML work — every step is
// idempotent — so it is not worth cross-locking the scanners.
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

// markOffline flags every currently-online asset under mount as offline.
// substr() prefix compare instead of LIKE: see markOfflineOutside.
func (g *MountGuard) markOffline(mount string) error {
	p := strings.TrimRight(mount, "/") + "/"
	res, err := g.db.Exec(
		`UPDATE assets SET offline=1 WHERE offline=0 AND substr(file_path,1,length(?))=?`, p, p)
	if err != nil {
		zap.L().Warn("mountguard: mark offline failed", zap.String("mount", mount), zap.Error(err))
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		zap.L().Info("mountguard: marked assets offline (drive unplugged)",
			zap.String("mount", mount), zap.Int64("count", n))
	}
	return nil
}

// markOnline flags every offline asset under mount back online.
func (g *MountGuard) markOnline(mount string) error {
	p := strings.TrimRight(mount, "/") + "/"
	res, err := g.db.Exec(
		`UPDATE assets SET offline=0 WHERE offline=1 AND substr(file_path,1,length(?))=?`, p, p)
	if err != nil {
		zap.L().Warn("mountguard: mark online failed", zap.String("mount", mount), zap.Error(err))
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		zap.L().Info("mountguard: marked assets online (drive reinserted)",
			zap.String("mount", mount), zap.Int64("count", n))
	}
	return nil
}

func toMountSet(mounts []string) map[string]bool {
	set := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		set[m] = true
	}
	return set
}
