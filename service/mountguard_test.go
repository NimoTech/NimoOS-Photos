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
	// A generic tracked removable mount: RAID-style 2-segment naming under
	// /media/ (NOT /media/devmon/, which is excluded entirely — see
	// TestMountGuard_DevmonMountsIgnoredEntirely below).
	mediaAsset := insertAsset(t, db, "/media/RAID_X/photo.jpg", "indexed")
	dataAsset := insertAsset(t, db, "/DATA/Gallery/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/RAID_X"}
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

	// Unplug: /media/RAID_X disappears from the mount snapshot.
	mounts = []string{}
	mg.checkOnce(context.Background())
	require.True(t, offlineFlag(mediaAsset), "/media/RAID_X 资产应被标记 offline")
	require.False(t, offlineFlag(dataAsset), "/DATA 资产不受影响")
	require.Equal(t, int32(0), watcherCalls.Load(), "拔出时不应触发恢复回调")
	require.Equal(t, int32(0), scanCalls.Load())
	require.Equal(t, int32(0), backfillCalls.Load())
	require.Equal(t, int32(0), ocrCalls.Load())

	// Replug: /media/RAID_X reappears.
	mounts = []string{"/media/RAID_X"}
	mg.checkOnce(context.Background())
	require.False(t, offlineFlag(mediaAsset), "重新插入后应恢复 offline=0")
	require.False(t, offlineFlag(dataAsset))
	require.Eventually(t, func() bool {
		return watcherCalls.Load() == 1 && scanCalls.Load() == 1 &&
			backfillCalls.Load() == 1 && ocrCalls.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "重新插入应各触发一次 watcher 重启/rescan/CLIP/OCR 补跑")
	require.Equal(t, "/media/RAID_X", scannedMount.Load())
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
	assetID := insertAsset(t, db, "/media/Z/photo.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return []string{"/media/Z"} }

	mg.AlignOnStartup()

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, assetID).Scan(&offline))
	require.Equal(t, 0, offline, "挂载点在启动时已存在,应恢复 offline=0")
}

// TestMountGuard_LikeMetacharSiblingMountsUnaffected: `_` is a LIKE wildcard,
// and real USB labels contain it (Kingston_DataTra). Unplugging
// /media/disk_A must not touch its sibling /media/diskXA, which
// a naive `LIKE 'disk_A/%'` prefix pattern would also match.
func TestMountGuard_LikeMetacharSiblingMountsUnaffected(t *testing.T) {
	db := makeTestDB(t)
	aAsset := insertAsset(t, db, "/media/disk_A/photo.jpg", "indexed")
	xAsset := insertAsset(t, db, "/media/diskXA/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/disk_A", "/media/diskXA"}
	mg.currentMounts = func() []string { return mounts }
	mg.AlignOnStartup()
	mg.lastMounts = toMountSet(mg.currentMounts())

	// Unplug disk_A only.
	mounts = []string{"/media/diskXA"}
	mg.checkOnce(context.Background())

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, aAsset).Scan(&offline))
	require.Equal(t, 1, offline, "disk_A 资产应被标 offline")
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, xAsset).Scan(&offline))
	require.Equal(t, 0, offline, "拔出 disk_A 不得波及仅 `_` 位不同的兄弟挂载 diskXA")

	// Same for the startup alignment direction: only disk_A absent.
	_, err := db.Exec(`UPDATE assets SET offline=0`)
	require.NoError(t, err)
	mg.currentMounts = func() []string { return []string{"/media/diskXA"} }
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
	_ = insertAsset(t, db, "/media/X/photo.jpg", "indexed")
	yAsset := insertAsset(t, db, "/media/Y/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/X", "/media/Y"}
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
	mounts = []string{"/media/Y"}
	mg.checkOnce(context.Background())
	mounts = []string{"/media/X", "/media/Y"}

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
	mounts = []string{"/media/X"}
	mg.checkOnce(context.Background())
	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, yAsset).Scan(&offline))
	require.Equal(t, 1, offline, "X 的恢复阻塞期间,Y 拔出仍须被标记 offline")

	// X bounces (unplug+replug) while its recovery is still in flight — the
	// re-trigger must be deduped, not stacked.
	mounts = []string{}
	mg.checkOnce(context.Background())
	mounts = []string{"/media/X"}
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
	mounts = []string{"/media/X"}
	mg.checkOnce(context.Background())
	require.Eventually(t, func() bool { return scanCalls.Load() == 2 },
		5*time.Second, 10*time.Millisecond, "上一轮恢复结束后,新的插回应能再次触发恢复")
}

// TestMountGuard_DevmonMountsIgnoredEntirely covers the product decision to
// stop indexing devmon's removable USB mounts (/media/devmon/<label>):
// MountGuard must treat them as if they don't exist at all — never counted as
// "present" for AlignOnStartup's restore pass, never diffed as an unplug/
// replug transition by checkOnce, and (defensively — after the startup purge
// there should be no such asset left) never flipped offline by the blanket
// markOfflineOutside pass either. This is the mirror image of
// TestMountGuard_UnplugMarksOfflineReplugRestoresAndTriggersRecovery, which
// covers the same lifecycle for a mount that IS tracked.
func TestMountGuard_DevmonMountsIgnoredEntirely(t *testing.T) {
	db := makeTestDB(t)
	asset := insertAsset(t, db, "/media/devmon/USB1/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/devmon/USB1"}
	mg.currentMounts = func() []string { return mounts }

	var watcherCalls, scanCalls, backfillCalls, ocrCalls atomic.Int32
	mg.SetWatcherRestart(func() { watcherCalls.Add(1) })
	mg.SetScanDir(func(m string) error { scanCalls.Add(1); return nil })
	mg.SetBackfill(func(ctx context.Context) error { backfillCalls.Add(1); return nil })
	mg.SetBackfillOCR(func(ctx context.Context) error { ocrCalls.Add(1); return nil })

	offlineFlag := func() bool {
		var v int
		require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, asset).Scan(&v))
		return v == 1
	}

	// Startup alignment with the devmon mount "present": must not be treated
	// as an online mount (it's excluded), and markOfflineOutside's blanket
	// /media/* sweep must not flip it offline either.
	mg.AlignOnStartup()
	require.False(t, offlineFlag(), "devmon 挂载点存在时资产不应被启动对齐波及")
	mg.lastMounts = toMountSet(mg.currentMounts())
	require.Empty(t, mg.lastMounts, "devmon 挂载点不应进入 MountGuard 的快照")

	// "Unplug": devmon mount disappears from the raw enumeration.
	mounts = []string{}
	mg.checkOnce(context.Background())
	require.False(t, offlineFlag(), "devmon 拔出不应标记 offline(清理后本不应存在此类资产,防御性排除)")
	require.Equal(t, int32(0), watcherCalls.Load())
	require.Equal(t, int32(0), scanCalls.Load())
	require.Equal(t, int32(0), backfillCalls.Load())
	require.Equal(t, int32(0), ocrCalls.Load())

	// "Replug": devmon mount reappears in the raw enumeration.
	mounts = []string{"/media/devmon/USB1"}
	mg.checkOnce(context.Background())
	require.False(t, offlineFlag())
	time.Sleep(50 * time.Millisecond) // give a would-be async recovery goroutine a chance to fire
	require.Equal(t, int32(0), watcherCalls.Load(), "devmon 挂载点重新出现不应触发任何恢复回调")
	require.Equal(t, int32(0), scanCalls.Load())
	require.Equal(t, int32(0), backfillCalls.Load())
	require.Equal(t, int32(0), ocrCalls.Load())
}

// TestMountGuard_AlignOnStartupNeverFlagsDevmonAssetsOffline is the narrow
// regression for markOfflineOutside's devmon exclusion in isolation: even
// with the devmon mount absent at startup (the common case — devmon assets
// shouldn't exist post-purge, but if one lingers), alignment must not mark it
// offline the way it would any other /media/* asset under an absent mount.
func TestMountGuard_AlignOnStartupNeverFlagsDevmonAssetsOffline(t *testing.T) {
	db := makeTestDB(t)
	asset := insertAsset(t, db, "/media/devmon/USB2/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return nil }

	mg.AlignOnStartup()

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, asset).Scan(&offline))
	require.Equal(t, 0, offline, "devmon 资产不应被启动对齐标记 offline")
}
