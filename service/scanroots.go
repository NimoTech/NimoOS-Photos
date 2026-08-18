package service

import (
	"os"
	"sort"
	"strings"
)

// procMountsPath is the kernel mount table; overridable in tests (unused in
// prod). EnumerateScanRoots reads it to discover user-accessible partitions.
const procMountsPath = "/proc/mounts"

// EnumerateScanRoots returns every directory Photos should scan: the system
// disk (/DATA) plus every currently-mounted *user* partition — RAID arrays
// (/media/RAID_*), manually-mounted drives (/media/Storage_*) and udev
// auto-mounted USB (/mnt/Disk-*). On read failure it degrades to just /DATA.
//
// It deliberately does NOT match every /media/* mount: the OS keeps its own
// root filesystem mounted at /media/root-ro and /media/root-rw, which are
// system partitions, not user data — scanning them would index OS files and
// spawn spurious "indexing" tasks.
func EnumerateScanRoots() []string {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return []string{"/DATA"}
	}
	return parseScanRoots(string(data))
}

// parseScanRoots is the pure, testable core of EnumerateScanRoots.
func parseScanRoots(mounts string) []string {
	set := map[string]bool{"/DATA": true}
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mp := unescapeMount(fields[1])
		// fuse.rclone cloud-drive mounts are neither scanned nor watched: inotify
		// events on FUSE are unreliable, and a full index would pull down every
		// cloud file (bandwidth/traffic); cloud-drive photos go through a
		// dedicated import feature instead. Only fuse.rclone is singled out —
		// other FUSE mounts like MergerFS (fuse.mergerfs) are first-class user
		// storage and must be kept.
		if fields[2] == rcloneFSType {
			continue
		}
		if isUserPartition(mp) {
			set[mp] = true
		}
	}
	roots := make([]string, 0, len(set))
	for mp := range set {
		roots = append(roots, mp)
	}
	sort.Strings(roots)
	return roots
}

// rcloneFSType is the /proc/mounts filesystem type rclone FUSE mounts report.
const rcloneFSType = "fuse.rclone"

// enumerateRcloneMounts returns the mount points of all currently-mounted
// rclone FUSE cloud drives, sorted. Used by the startup purge of legacy
// cloud-drive assets (they must never be indexed; see parseScanRoots).
func enumerateRcloneMounts() []string {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return nil
	}
	return parseRcloneMounts(string(data))
}

// parseRcloneMounts is the pure, testable core of enumerateRcloneMounts.
func parseRcloneMounts(mounts string) []string {
	var out []string
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != rcloneFSType {
			continue
		}
		out = append(out, unescapeMount(fields[1]))
	}
	sort.Strings(out)
	return out
}

// systemMounts are OS mount points that sit under /media or /mnt (so they'd
// otherwise pass the prefix test) but must never be scanned or indexed:
// /media/root-ro & /media/root-rw are the overlayfs root layers, /mnt/overlay
// is the merged root, /mnt/metadata is the system metadata partition.
var systemMounts = map[string]bool{
	"/media/root-ro": true,
	"/media/root-rw": true,
	"/mnt/overlay":   true,
	"/mnt/metadata":  true,
}

// excludedMountPrefixes are mount-point prefixes that must never be scanned or
// indexed even though they'd otherwise pass the /media or /mnt prefix test.
//
// /media/devmon/<label> is where devmon (the removable-media automounter)
// mounts a plugged-in USB drive or SD card — distinct from the mdadm RAID
// arrays at /media/RAID_* and the udev/LocalStorage automounts at
// /mnt/Disk-*, both of which stay in scope. Product decision: USB drives are
// ephemeral, low-trust storage that should never enter the photo library, so
// every /media/devmon/* mount point is excluded from scanning
// (EnumerateScanRoots), MountGuard's offline tracking, and the startup purge
// of any legacy devmon assets left over from before this decision.
//
// Known limitation (accepted): if devmon is disabled/uninstalled, LocalStorage
// can grab the same physical USB drive and (re)mount it under
// /mnt/Disk-<uuid> instead — a mount-naming race this service does not control
// and cannot distinguish from a "real" fixed disk. Once that happens the drive
// is, from Photos' point of view, an ordinary /mnt/Disk-* mount and IS
// scanned again.
var excludedMountPrefixes = []string{"/media/devmon/"}

// IsExcludedMount reports whether mp is a mount point Photos must never treat
// as user data to scan/track: an exact-match OS system mount (systemMounts), a
// mount under one of excludedMountPrefixes (currently just devmon's
// removable-media namespace), or a mount point sitting under a ".snapshots"
// directory component (see isInSnapshotsDir) — btrbk/snapper mount each
// read-only hourly btrfs snapshot subvolume as its own /proc/mounts entry
// under "<volume mountpoint>/.snapshots/<ts>/", so without this check
// EnumerateScanRoots would hand them back as ordinary scan/watch roots (this
// was the actual root cause of snapshot subvolumes getting indexed as if they
// were the live library — the walk-time hidden-dir skip only ever fires for a
// directory encountered *during* a walk, never for the walk's own root).
// isUserPartition, MountGuard's mount-set snapshotting, and the startup
// devmon-asset purge all call this single function so they stay in lockstep
// by construction — a new exclusion only needs to be added here once.
func IsExcludedMount(mp string) bool {
	if systemMounts[mp] {
		return true
	}
	for _, p := range excludedMountPrefixes {
		if strings.HasPrefix(mp, p) {
			return true
		}
	}
	if isInSnapshotsDir(mp) {
		return true
	}
	return false
}

// isUserPartition reports whether a mount point is a user-accessible data
// partition Photos should scan. We do NOT whitelist naming prefixes: RAID
// arrays (/media/RAID_*), manual mounts (/media/Storage_* or user-named
// /media/<name>), MergerFS and custom mounts can all use arbitrary names, so a
// prefix whitelist would miss real user data. Instead we accept any mount under
// /media or /mnt and exclude the known system/devmon mounts via IsExcludedMount.
func isUserPartition(mp string) bool {
	if IsExcludedMount(mp) {
		return false
	}
	return strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/")
}

// unescapeMount decodes the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses in mount-point fields.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// VolumeRootForPath returns the longest entry in roots that path lives under
// (path equals the root, or starts with root+"/"), or "" if none match. Used
// by TrashService to pin a per-asset .trash directory to the same filesystem
// the asset already lives on (see trash.go) so the trash-move rename() is
// always same-device and never hits EXDEV.
//
// Longest-match wins so a more specific root (e.g. a nested bind mount) beats
// a shorter ancestor root if both happen to be present in roots.
func VolumeRootForPath(path string, roots []string) string {
	best := ""
	for _, r := range roots {
		r = strings.TrimRight(r, "/")
		if r == "" {
			continue
		}
		if path == r || strings.HasPrefix(path, r+"/") {
			if len(r) > len(best) {
				best = r
			}
		}
	}
	return best
}
