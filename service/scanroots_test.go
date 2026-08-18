package service

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseScanRoots(t *testing.T) {
	mounts := `/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw,nosuid 0 0
tmpfs /run tmpfs rw,nosuid 0 0
/dev/sda2 /DATA ext4 rw,relatime 0 0
/dev/md0 /media/RAID_Photos ext4 rw 0 0
/dev/sdb1 /media/Storage_usb0 vfat rw 0 0
/dev/sdc1 /mnt/Disk-1a2b3c4d ext4 rw 0 0
/dev/sdd1 /media/RAID_With\040Space ext4 rw 0 0
/dev/sde1 /media/MyOwnName ext4 rw 0 0
/dev/sdf1 /mnt/merge fuse.mergerfs rw 0 0
/dev/nvme0n1p3 /media/root-ro ext4 rw 0 0
/dev/nvme0n1p4 /media/root-rw ext4 rw 0 0
/dev/nvme0n1p7 /mnt/overlay ext4 rw 0 0
/dev/nvme0n1p6 /mnt/metadata ext4 rw 0 0
`
	got := parseScanRoots(mounts)
	// Denylist, not whitelist: any /media//mnt mount is a user partition EXCEPT
	// the known system mounts. So a custom-named drive (/media/MyOwnName) and a
	// MergerFS mount (/mnt/merge) are included, while /media/root-ro,
	// /media/root-rw, /mnt/overlay and /mnt/metadata are excluded.
	want := []string{
		"/DATA",
		"/media/MyOwnName",
		"/media/RAID_Photos",
		"/media/RAID_With Space",
		"/media/Storage_usb0",
		"/mnt/Disk-1a2b3c4d",
		"/mnt/merge",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseScanRoots\n got=%v\nwant=%v", got, want)
	}
}

func TestParseScanRootsAlwaysIncludesDATAOnce(t *testing.T) {
	got := parseScanRoots("proc /proc proc rw 0 0\n")
	if len(got) != 1 || got[0] != "/DATA" {
		t.Fatalf("want [/DATA], got %v", got)
	}
}

// TestIsExcludedMountAndUserPartition: USB drives (devmon namespace) are
// excluded entirely, RAID/single-disk storage/arbitrary user mounts are
// retained, and system mount points are still rejected.
func TestIsExcludedMountAndUserPartition(t *testing.T) {
	cases := []struct {
		mp       string
		excluded bool
		user     bool
	}{
		{"/media/devmon/sdg1-usb-Kingston_DataTra", true, false},
		{"/media/devmon/x", true, false},
		{"/media/RAID_0", false, true},
		{"/media/MyVolume", false, true},
		{"/mnt/Disk-abc12345", false, true},
		{"/media/root-ro", true, false},
		{"/mnt/overlay", true, false},
		{"/home/user", false, false}, // prefix doesn't match, not a user partition but also not on the "exclusion list"
		// btrbk/snapper mounts each read-only hourly snapshot subvolume as its
		// own /proc/mounts entry: once such an entry is treated as an
		// ordinary user partition, it bypasses the hidden-directory skip rule
		// that only kicks in during a walk (that rule only blocks when
		// "encountered during traversal", never blocking the case where "the
		// walk's own root is already inside .snapshots").
		{"/media/RAID_0/.snapshots/20260716T200714Z_auto-hourly", true, false},
		// Only a path-substring hit, not an actual .snapshots directory
		// component, must not be falsely blocked.
		{"/media/RAID_0/my.snapshots.backup", false, true},
	}
	for _, c := range cases {
		if got := IsExcludedMount(c.mp); got != c.excluded {
			t.Errorf("IsExcludedMount(%q) = %v, want %v", c.mp, got, c.excluded)
		}
		if got := isUserPartition(c.mp); got != c.user {
			t.Errorf("isUserPartition(%q) = %v, want %v", c.mp, got, c.user)
		}
	}
}

// TestParseScanRootsExcludesRcloneFuse: rclone cloud-drive mounts
// (fuse.rclone) are not in scope for scanning/watching — inotify on FUSE is
// unreliable, and indexing a cloud drive would pull down every remote file;
// MergerFS (fuse.mergerfs) is first-class user storage and must be kept.
func TestParseScanRootsExcludesRcloneFuse(t *testing.T) {
	mounts := strings.Join([]string{
		"/dev/sda1 /DATA ext4 rw 0 0",
		"dropbox: /mnt/yu.wu_dropbox_1782892446 fuse.rclone rw 0 0",
		"pool /mnt/pool fuse.mergerfs rw 0 0",
		"/dev/md0 /media/RAID_0 btrfs rw 0 0",
	}, "\n")
	roots := parseScanRoots(mounts)
	require.NotContains(t, roots, "/mnt/yu.wu_dropbox_1782892446", "fuse.rclone mounts must be excluded")
	require.Contains(t, roots, "/mnt/pool", "fuse.mergerfs must be retained")
	require.Contains(t, roots, "/media/RAID_0")
	require.Contains(t, roots, "/DATA")
}

// TestParseRcloneMounts: enumerates rclone mount points (used by the startup
// cleanup of legacy assets mistakenly indexed).
func TestParseRcloneMounts(t *testing.T) {
	mounts := strings.Join([]string{
		"/dev/sda1 /DATA ext4 rw 0 0",
		"dropbox: /mnt/yu.wu_dropbox_1782892446 fuse.rclone rw 0 0",
		"gdrive: /mnt/a_gdrive_9\\040x fuse.rclone rw 0 0", // mount point contains an escaped space
		"pool /mnt/pool fuse.mergerfs rw 0 0",
	}, "\n")
	require.Equal(t, []string{"/mnt/a_gdrive_9 x", "/mnt/yu.wu_dropbox_1782892446"},
		parseRcloneMounts(mounts))
}

// TestVolumeRootForPath covers the longest-prefix matching TrashService
// relies on to pin an asset's trash directory to its own volume (see
// trash.go's trashDirFor) instead of a single fixed root — the fix for the
// 2026-08-18 delete-chain EXDEV diagnosis.
func TestVolumeRootForPath(t *testing.T) {
	roots := []string{"/DATA", "/media/RAID_0", "/media/RAID_0/.snapshots"}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"under /DATA", "/DATA/Gallery/a.jpg", "/DATA"},
		{"under /media/RAID_0", "/media/RAID_0/demo/photo.jpg", "/media/RAID_0"},
		{"exact root match", "/media/RAID_0", "/media/RAID_0"},
		{"longest match wins for nested roots", "/media/RAID_0/.snapshots/2026/x.jpg", "/media/RAID_0/.snapshots"},
		{"no match returns empty", "/mnt/Disk-usb/x.jpg", ""},
		{"sibling with shared prefix is not a false match", "/media/RAID_01/x.jpg", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, VolumeRootForPath(c.path, roots))
		})
	}
}
