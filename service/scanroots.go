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
		if len(fields) < 2 {
			continue
		}
		mp := unescapeMount(fields[1])
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

// isUserPartition reports whether a mount point is a user-accessible data
// partition Photos should scan. We do NOT whitelist naming prefixes: RAID
// arrays (/media/RAID_*), manual mounts (/media/Storage_* or user-named
// /media/<name>), MergerFS and custom mounts can all use arbitrary names, so a
// prefix whitelist would miss real user data. Instead we accept any mount under
// /media or /mnt and exclude the known system mounts above.
func isUserPartition(mp string) bool {
	if systemMounts[mp] {
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
