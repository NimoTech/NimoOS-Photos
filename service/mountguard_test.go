package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMountGuard_UnplugMarksOfflineReplugRestoresAndTriggersRecovery covers the
// core lifecycle: a removable mount vanishing from the poll snapshot flags its
// assets offline (and only its assets — /DATA is untouched), and it
// reappearing flags them back online and fires the injected recovery hooks
// (watcher restart, scoped rescan, CLIP + OCR backfill) exactly once each.
func TestMountGuard_UnplugMarksOfflineReplugRestoresAndTriggersRecovery(t *testing.T) {
	db := makeTestDB(t)
	// devmon-style automount naming: /media/<agent>/<label>/<file...>, matching
	// the real-world layout mountPrefixFromAssetPath is built around.
	mediaAsset := insertAsset(t, db, "/media/devmon/X/photo.jpg", "indexed")
	dataAsset := insertAsset(t, db, "/DATA/Gallery/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mounts := []string{"/media/devmon/X"}
	mg.currentMounts = func() []string { return mounts }

	var watcherCalls, scanCalls, backfillCalls, ocrCalls int
	var scannedMount string
	mg.SetWatcherRestart(func() { watcherCalls++ })
	mg.SetScanDir(func(m string) error { scanCalls++; scannedMount = m; return nil })
	mg.SetBackfill(func(ctx context.Context) error { backfillCalls++; return nil })
	mg.SetBackfillOCR(func(ctx context.Context) error { ocrCalls++; return nil })

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
	require.Equal(t, 0, watcherCalls, "拔出时不应触发恢复回调")
	require.Equal(t, 0, scanCalls)
	require.Equal(t, 0, backfillCalls)
	require.Equal(t, 0, ocrCalls)

	// Replug: /media/devmon/X reappears.
	mounts = []string{"/media/devmon/X"}
	mg.checkOnce(context.Background())
	require.False(t, offlineFlag(mediaAsset), "重新插入后应恢复 offline=0")
	require.False(t, offlineFlag(dataAsset))
	require.Equal(t, 1, watcherCalls, "重新插入应重启 watcher 一次")
	require.Equal(t, 1, scanCalls, "重新插入应对该挂载点 rescan 一次")
	require.Equal(t, "/media/devmon/X", scannedMount)
	require.Equal(t, 1, backfillCalls, "重新插入应触发一次 CLIP backfill")
	require.Equal(t, 1, ocrCalls, "重新插入应触发一次 OCR backfill")
}

// TestMountGuard_AlignOnStartupCatchesUnplugWhileServiceWasDown covers the
// "drive was unplugged while the service wasn't running" case: on startup
// there's no prior snapshot to diff against, so AlignOnStartup must derive the
// mount-point candidates directly from existing /media/* asset paths and mark
// them offline if the mount isn't currently present.
func TestMountGuard_AlignOnStartupCatchesUnplugWhileServiceWasDown(t *testing.T) {
	db := makeTestDB(t)
	assetID := insertAsset(t, db, "/media/devmon/sdg1-usb-Kingston_DataTra/photo.jpg", "indexed")

	mg := NewMountGuard(db)
	mg.currentMounts = func() []string { return nil } // 挂载点在启动时不存在

	mg.AlignOnStartup()

	var offline int
	require.NoError(t, db.QueryRow(`SELECT offline FROM assets WHERE id=?`, assetID).Scan(&offline))
	require.Equal(t, 1, offline, "服务停机期间拔出的盘应在启动对齐时被标记 offline")
}

// TestMountGuard_AlignOnStartupRestoresOnlineWhenMountPresent covers the
// inverse: an asset previously marked offline whose drive IS present at
// startup (e.g. it was replugged while the service was down) must come back
// online during the alignment pass.
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
