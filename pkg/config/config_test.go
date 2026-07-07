package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// loadConfigFromINI 写临时 ini 文件并调用 Init，返回加载后的 Cfg。
func loadConfigFromINI(t *testing.T, ini string) *Config {
	t.Helper()
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, os.WriteFile(cf, []byte(ini), 0644))
	require.NoError(t, Init(cf, ini))
	return Cfg
}

// TestWatchDirsEmptyMeansAuto：watchdirs 不配置（或置空）⇒ Cfg.WatchDirs 为空，
// watcher 进入自动模式（范围=EnumerateScanRoots）；显式配置 ⇒ 手工清单原样保留。
func TestWatchDirsEmptyMeansAuto(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.Empty(t, cfg.WatchDirs, "未配置 watchdirs 必须为空（自动模式），不得回填旧三目录默认")

	cfg = loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\nWatchDirs=/DATA/Gallery,/DATA/Media\n")
	require.Equal(t, []string{"/DATA/Gallery", "/DATA/Media"}, cfg.WatchDirs)
}

// 配置文件无新 key 时，三个新开关默认 true（与 FacesEnabled 同语义）。
func TestNewFlagsDefaultTrue(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.True(t, Cfg.ScenesEnabled)
	require.True(t, Cfg.OCREnabled)
	require.True(t, Cfg.SmartViewEnabled)
}

// Save 写盘后重新 Init 能读回完全一致的值（含 false）。
func TestSaveRoundTrip(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.NoError(t, Save(Settings{
		WatchDirs:        []string{"/tmp/a"},
		RetentionDays:    7,
		FacesEnabled:     false,
		ScenesEnabled:    false,
		OCREnabled:       true,
		SmartViewEnabled: false,
	}))
	require.NoError(t, Init(cf, "[photos]\n"))
	require.Equal(t, []string{"/tmp/a"}, Cfg.WatchDirs)
	require.Equal(t, 7, Cfg.RetentionDays)
	require.False(t, Cfg.FacesEnabled)
	require.False(t, Cfg.ScenesEnabled)
	require.True(t, Cfg.OCREnabled)
	require.False(t, Cfg.SmartViewEnabled)
}

func TestScanIntervalDefaultAndSave(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "photos.conf")
	sample := "[photos]\nWatchDirs = /DATA/Gallery\n"
	if err := Init(cf, sample); err != nil {
		t.Fatal(err)
	}
	if Cfg.ScanInterval != 1440 {
		t.Fatalf("default ScanInterval=1440, got %d", Cfg.ScanInterval)
	}
	if err := Save(Settings{WatchDirs: []string{"/DATA/Gallery"}, ScanInterval: 360}); err != nil {
		t.Fatal(err)
	}
	if Cfg.ScanInterval != 360 {
		t.Fatalf("after Save ScanInterval=360, got %d", Cfg.ScanInterval)
	}
	if err := Init(cf, sample); err != nil {
		t.Fatal(err)
	}
	if Cfg.ScanInterval != 360 {
		t.Fatalf("reloaded ScanInterval=360, got %d", Cfg.ScanInterval)
	}
}
