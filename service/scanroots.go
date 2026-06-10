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
// disk (/DATA) plus every currently-mounted user partition under /media/*
// (RAID arrays, manually-mounted drives) and /mnt/Disk-* (udev auto-mounted
// USB). On read failure it degrades to just /DATA.
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
		if strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/Disk-") {
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

// unescapeMount decodes the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses in mount-point fields.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
