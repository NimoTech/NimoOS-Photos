package service

import (
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// snapshotsDirName is the fixed (lower-case only — btrfs snapshot tooling
// never varies it) directory name btrbk/snapper use to hold read-only hourly
// snapshot subvolumes, mounted at "<volume mountpoint>/.snapshots/<ts>/...".
// A btrfs snapshot is a full copy-on-write copy of the entire tree it
// snapshots, so if Photos ever indexes anything under it, every retained
// snapshot re-indexes the same photo again — and the rows churn (added/
// removed) every time snapshots rotate.
const snapshotsDirName = ".snapshots"

// isInSnapshotsDir reports whether path has ".snapshots" as one of its path
// components — an exact path-segment match, never a substring match, so a
// directory merely named similarly (e.g. "/media/RAID_0/my.snapshots.backup/
// a.jpg") is never mistaken for the real thing.
//
// This is the single shared predicate every ingestion entry point must call
// so a snapshot subvolume can never slip in through a path this function
// wasn't wired into:
//   - mount-root enumeration (IsExcludedMount, scanroots.go) — the actual
//     root cause of the original bug: btrbk/snapper mount each snapshot
//     subvolume as its own /proc/mounts entry under /media/RAID_*/.snapshots/
//     <ts>/, so EnumerateScanRoots was handing them back as ordinary scan
//     roots, entirely bypassing the walk-time hidden-dir skip below (that
//     skip only fires for a directory encountered *during* a walk, never for
//     the walk's own root).
//   - directory walk (walkSupported, indexer.go / addRecursiveWatch,
//     watcher.go) — both the root itself and any nested ".snapshots"
//     component encountered mid-walk.
//   - single-path enqueue (Enqueue / EnqueueWithBatch / MarkAndReserve,
//     indexer.go) — covers MessageBus create events, the asset-move route,
//     and any other direct-path ingestion that does not go through a walk.
//   - processFileInternal (indexer.go) — the final choke point every path
//     eventually reaches (via the worker queue or ScanDirectory's synchronous
//     loop), so even a future ingestion path that forgets to call this
//     function directly still cannot write a snapshot-sourced asset row.
//   - the startup cleanup sweep (pruneSnapshotAssets, below) — re-validates
//     every LIKE-matched candidate before deleting it.
func isInSnapshotsDir(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == snapshotsDirName {
			return true
		}
	}
	return false
}

// pruneSnapshotAssets removes any indexed asset whose file_path lives under a
// ".snapshots" directory component — cleanup for contamination indexed before
// the filters described on isInSnapshotsDir existed (or from any as-yet
// unnoticed bypass). Runs at startup (service.go), mirroring
// pruneSystemMountAssets / pruneRcloneMountAssets. Reuses RemoveByPath so CLIP
// vectors, thumbnails and (via FK cascade) face rows are all cleaned
// consistently — this never issues a bare DELETE that would leave orphans.
func (ix *Indexer) pruneSnapshotAssets() {
	rows, err := ix.db.Query(`SELECT file_path FROM assets WHERE file_path LIKE ?`, "%/"+snapshotsDirName+"/%")
	if err != nil {
		zap.L().Warn("pruneSnapshotAssets: query failed", zap.Error(err))
		return
	}
	var candidates []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			candidates = append(candidates, p)
		}
	}
	rows.Close()

	removed := 0
	for _, p := range candidates {
		// The LIKE above is a coarse pre-filter (SQL has no path-segment
		// matching); re-validate every candidate with the authoritative
		// component-based matcher before deleting anything — belt-and-
		// suspenders against any LIKE-pattern edge case ever creeping in.
		if !isInSnapshotsDir(p) {
			continue
		}
		ix.RemoveByPath(p)
		removed++
	}
	zap.L().Info("pruneSnapshotAssets: cleaned up assets indexed from btrfs .snapshots directories",
		zap.Int("removed", removed))
}
