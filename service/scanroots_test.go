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

// TestIsExcludedMountAndUserPartition:U 盘(devmon 命名空间)整体排除,
// RAID/单盘 storage/任意用户挂载保留,系统挂载点仍被拒。
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
		{"/home/user", false, false}, // 前缀不符,非用户分区但也非"排除名单"
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

// TestParseScanRootsExcludesRcloneFuse:rclone 云盘挂载(fuse.rclone)不进
// 扫描/监控范围——FUSE 上 inotify 不可靠,索引云盘会把远端文件全量拉下来;
// MergerFS(fuse.mergerfs)是一等用户存储,必须保留。
func TestParseScanRootsExcludesRcloneFuse(t *testing.T) {
	mounts := strings.Join([]string{
		"/dev/sda1 /DATA ext4 rw 0 0",
		"dropbox: /mnt/yu.wu_dropbox_1782892446 fuse.rclone rw 0 0",
		"pool /mnt/pool fuse.mergerfs rw 0 0",
		"/dev/md0 /media/RAID_0 btrfs rw 0 0",
	}, "\n")
	roots := parseScanRoots(mounts)
	require.NotContains(t, roots, "/mnt/yu.wu_dropbox_1782892446", "fuse.rclone 挂载必须被排除")
	require.Contains(t, roots, "/mnt/pool", "fuse.mergerfs 必须保留")
	require.Contains(t, roots, "/media/RAID_0")
	require.Contains(t, roots, "/DATA")
}

// TestParseRcloneMounts:rclone 挂载点枚举(供启动清理历史误入库资产用)。
func TestParseRcloneMounts(t *testing.T) {
	mounts := strings.Join([]string{
		"/dev/sda1 /DATA ext4 rw 0 0",
		"dropbox: /mnt/yu.wu_dropbox_1782892446 fuse.rclone rw 0 0",
		"gdrive: /mnt/a_gdrive_9\\040x fuse.rclone rw 0 0", // 挂载点含空格转义
		"pool /mnt/pool fuse.mergerfs rw 0 0",
	}, "\n")
	require.Equal(t, []string{"/mnt/a_gdrive_9 x", "/mnt/yu.wu_dropbox_1782892446"},
		parseRcloneMounts(mounts))
}
