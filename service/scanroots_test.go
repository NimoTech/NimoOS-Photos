package service

import (
	"reflect"
	"testing"
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
