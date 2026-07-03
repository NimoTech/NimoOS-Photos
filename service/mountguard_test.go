package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMountGuard_UnplugMarksOfflineReplugRestoresAndTriggersRecovery covers the
// core lifecycle: a removable mount vanishing from the poll snapshot flags its
// assets offline (and only its assets — /DATA is untouched), and it
// reappearing flags them back online and fires the injected recovery hooks
// (watcher restart, scoped rescan, CLIP + OCR backfill) exactly once each.
// Recovery runs asynchronously (see spawnRecovery), so hook assertions poll.
func TestMountGuard_UnplugMarksOfflineReplugRestoresAndTriggersRecovery(t *testing.T) {
	db := makeTestDB(t)
	// devmon-style automount naming: /media/<agent>/<label>/<file...>.
	mediaAsset := insertAsset(t, db, "/media/devmon/X/photo.jpg", "indexed")
	dataAsset := insertAsset(t, db, "/DATA/Gallery/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/devmon/X"}
	mg.currentMounts = func() []string { return mounts }

	var watcherCalls, scanCalls, backfillCalls, ocrCalls atomic.Int32
	var scannedMount atomic.Value
	mg.SetWatcherRestart(func() { watcherCalls.Add(1) })
	mg.SetScanDir(func(m string) error { scanCalls.Add(1); scannedMount.Store(m); return nil })
	mg.SetBackfill(func(ctx context.Context) error { backfillCalls.Add(1); return nil })
	mg.SetBackfillOCR(func(ctx context.Context) error { ocrCalls.Add(1); return nil })

	// Seed the guard's snapshot as if it had just started with the drive present.
	mg.AlignOnStartup()
	mg.lastMounts = toMountSet(mg.currentMounts())

	offlineFlag := func(id string) bool {
		var v int
		require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, id).Scan(&v))
		return v == 1
	}
	require.False(t, offlineFlag(mediaAsset))
	require.False(t, offlineFlag(dataAsset))

	// Unplug: /media/devmon/X disappears from the mount snapshot.
	mounts = []string{}
	mg.checkOnce(context.Background())
	require.True(t, offlineFlag(mediaAsset), "/media/devmon/X 资产应被标记 offline")
	require.False(t, offlineFlag(dataAsset), "/DATA 资产不受影响")
	require.Equal(t, int32(0), watcherCalls.Load(), "拔出时不应触发恢复回调")
	require.Equal(t, int32(0), scanCalls.Load())
	require.Equal(t, int32(0), backfillCalls.Load())
	require.Equal(t, int32(0), ocrCalls.Load())

	// Replug: /media/devmon/X reappears.
	mounts = []string{"/media/devmon/X"}
	mg.checkOnce(context.Background())
	require.False(t, offlineFlag(mediaAsset), "重新插入后应恢复 offline=0")
	require.False(t, offlineFlag(dataAsset))
	require.Eventually(t, func() bool {
		return watcherCalls.Load() == 1 && scanCalls.Load() == 1 &&
			backfillCalls.Load() == 1 && ocrCalls.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "重新插入应各触发一次 watcher 重启/rescan/CLIP/OCR 补跑")
	require.Equal(t, "/media/devmon/X", scannedMount.Load())
}

// TestMountGuard_AlignOnStartupCatchesUnplugWhileServiceWasDown covers the
// "drive was unplugged while the service wasn't running" case: on startup
// there's no prior snapshot to diff against, so alignment must flag offline
// every /media/* asset not under any currently-present mount. The 2-segment
// mount path (/media/Y) is deliberate — a previous implementation derived
// mount points from asset paths with a fixed 3-segment split and missed
// 2-level mounts entirely; this is its regression test.
func TestMountGuard_AlignOnStartupCatchesUnplugWhileServiceWasDown(t *testing.T) {
	db := makeTestDB(t)
	assetID := insertAsset(t, db, "/media/Y/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return nil } // /media/Y 挂载点在启动时不存在

	mg.AlignOnStartup()

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, assetID).Scan(&offline))
	require.Equal(t, 1, offline, "服务停机期间拔出的盘应在启动对齐时被标记 offline")
}

// TestMountGuard_AlignOnStartupTwoSegmentMountStaysOnline is the other half of
// the 2-segment-mount regression (real machine: /media/RAID_0): when the
// 2-level mount IS present at startup, alignment must NOT flag its assets
// offline — the old fixed-3-segment derivation treated /media/RAID_0/DCIM as
// the "mount point", found it absent from the mount set, and permanently
// ghosted online RAID assets with no runtime transition to ever heal them.
func TestMountGuard_AlignOnStartupTwoSegmentMountStaysOnline(t *testing.T) {
	db := makeTestDB(t)
	shallow := insertAsset(t, db, "/media/RAID_0/photo.jpg", "indexed")
	deep := insertAsset(t, db, "/media/RAID_0/DCIM/2024/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return []string{"/media/RAID_0"} }

	mg.AlignOnStartup()

	for _, id := range []string{shallow, deep} {
		var offline int
		require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, id).Scan(&offline))
		require.Equal(t, 0, offline, "二级挂载点在位时其资产不得被启动对齐误标 offline")
	}

	// And the inverse: once /media/RAID_0 is really absent, both go offline.
	mg.currentMounts = func() []string { return nil }
	mg.AlignOnStartup()
	for _, id := range []string{shallow, deep} {
		var offline int
		require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, id).Scan(&offline))
		require.Equal(t, 1, offline, "二级挂载点缺席时其资产必须被标 offline")
	}
}

// TestMountGuard_AlignOnStartupRestoresOnlineWhenMountPresent covers the
// inverse startup case: an asset previously marked offline whose drive IS
// present at startup (e.g. it was replugged while the service was down) must
// come back online during the alignment pass.
func TestMountGuard_AlignOnStartupRestoresOnlineWhenMountPresent(t *testing.T) {
	db := makeTestDB(t)
	assetID := insertAsset(t, db, "/media/devmon/Z/photo.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return []string{"/media/devmon/Z"} }

	mg.AlignOnStartup()

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, assetID).Scan(&offline))
	require.Equal(t, 0, offline, "挂载点在启动时已存在,应恢复 offline=0")
}

// TestMountGuard_LikeMetacharSiblingMountsUnaffected: `_` is a LIKE wildcard,
// and real USB labels contain it (Kingston_DataTra). Unplugging
// /media/devmon/disk_A must not touch its sibling /media/devmon/diskXA, which
// a naive `LIKE 'disk_A/%'` prefix pattern would also match.
func TestMountGuard_LikeMetacharSiblingMountsUnaffected(t *testing.T) {
	db := makeTestDB(t)
	aAsset := insertAsset(t, db, "/media/devmon/disk_A/photo.jpg", "indexed")
	xAsset := insertAsset(t, db, "/media/devmon/diskXA/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/devmon/disk_A", "/media/devmon/diskXA"}
	mg.currentMounts = func() []string { return mounts }
	mg.AlignOnStartup()
	mg.lastMounts = toMountSet(mg.currentMounts())

	// Unplug disk_A only.
	mounts = []string{"/media/devmon/diskXA"}
	mg.checkOnce(context.Background())

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, aAsset).Scan(&offline))
	require.Equal(t, 1, offline, "disk_A 资产应被标 offline")
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, xAsset).Scan(&offline))
	require.Equal(t, 0, offline, "拔出 disk_A 不得波及仅 `_` 位不同的兄弟挂载 diskXA")

	// Same for the startup alignment direction: only disk_A absent.
	_, err := db.Exec(`UPDATE assets SET offline=0`)
	require.NoError(t, err)
	mg.currentMounts = func() []string { return []string{"/media/devmon/diskXA"} }
	mg.AlignOnStartup()
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, aAsset).Scan(&offline))
	require.Equal(t, 1, offline)
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, xAsset).Scan(&offline))
	require.Equal(t, 0, offline, "启动对齐同样不得因 LIKE 元字符误伤兄弟挂载")
}

// TestMountGuard_RecoveryIsAsyncAndDeduped: a long recovery (ScanDirectory can
// run for hours) must not block checkOnce — another drive unplugged meanwhile
// still gets flagged on the next tick. And if the same mount bounces while its
// recovery is still in flight, the in-flight dedup drops the re-trigger
// instead of stacking a concurrent duplicate scan.
func TestMountGuard_RecoveryIsAsyncAndDeduped(t *testing.T) {
	db := makeTestDB(t)
	_ = insertAsset(t, db, "/media/devmon/X/photo.jpg", "indexed")
	yAsset := insertAsset(t, db, "/media/devmon/Y/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/devmon/X", "/media/devmon/Y"}
	mg.currentMounts = func() []string { return mounts }
	mg.AlignOnStartup()
	mg.lastMounts = toMountSet(mg.currentMounts())

	var scanCalls atomic.Int32
	scanStarted := make(chan struct{}, 4)
	release := make(chan struct{})
	mg.SetScanDir(func(m string) error {
		scanCalls.Add(1)
		scanStarted <- struct{}{}
		<-release // simulate an hours-long ScanDirectory
		return nil
	})

	// X unplugged, then replugged → recovery starts and blocks inside scanDir.
	mounts = []string{"/media/devmon/Y"}
	mg.checkOnce(context.Background())
	mounts = []string{"/media/devmon/X", "/media/devmon/Y"}

	done := make(chan struct{})
	go func() { mg.checkOnce(context.Background()); close(done) }()
	select {
	case <-done: // checkOnce must return without waiting for scanDir
	case <-time.After(5 * time.Second):
		t.Fatal("checkOnce 被恢复序列阻塞:恢复必须在独立 goroutine 中执行")
	}
	<-scanStarted // recovery for X is now in flight and blocked

	// While X's recovery is blocked, Y is unplugged — the poll loop must still
	// be able to flag it.
	mounts = []string{"/media/devmon/X"}
	mg.checkOnce(context.Background())
	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, yAsset).Scan(&offline))
	require.Equal(t, 1, offline, "X 的恢复阻塞期间,Y 拔出仍须被标记 offline")

	// X bounces (unplug+replug) while its recovery is still in flight — the
	// re-trigger must be deduped, not stacked.
	mounts = []string{}
	mg.checkOnce(context.Background())
	mounts = []string{"/media/devmon/X"}
	mg.checkOnce(context.Background())
	time.Sleep(50 * time.Millisecond) // give a would-be duplicate goroutine time to run
	require.Equal(t, int32(1), scanCalls.Load(), "恢复进行中同一挂载点的重复触发必须被去重")

	// Release the first recovery and wait for its goroutine to fully retire
	// (in-flight flag cleared) — only then can a fresh bounce trigger again.
	close(release)
	require.Eventually(t, func() bool {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		return len(mg.recovering) == 0
	}, 5*time.Second, 5*time.Millisecond, "释放后上一轮恢复应退场")

	mounts = []string{}
	mg.checkOnce(context.Background())
	mounts = []string{"/media/devmon/X"}
	mg.checkOnce(context.Background())
	require.Eventually(t, func() bool { return scanCalls.Load() == 2 },
		5*time.Second, 10*time.Millisecond, "上一轮恢复结束后,新的插回应能再次触发恢复")
}
