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

// isUserPartition reports whether a mount point is a user-accessible data
// partition Photos should scan. LocalStorage names user mounts predictably:
// RAID arrays at /media/RAID_<name>, manually-mounted drives at
// /media/Storage_<...>, and udev auto-mounted USB partitions at
// /mnt/Disk-<uuid>. Everything else under /media (root-ro, root-rw) is a
// system mount and is excluded.
func isUserPartition(mp string) bool {
	return strings.HasPrefix(mp, "/media/RAID_") ||
		strings.HasPrefix(mp, "/media/Storage_") ||
		strings.HasPrefix(mp, "/mnt/Disk-")
}

// unescapeMount decodes the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses in mount-point fields.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
