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

// 配置文件无新 key 时，新开关默认 true（与 FacesEnabled 同语义）。
func TestNewFlagsDefaultTrue(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.True(t, Cfg.ScenesEnabled)
	require.True(t, Cfg.OCREnabled)
	require.True(t, Cfg.SmartViewEnabled)
	require.True(t, Cfg.AestheticEnabled)
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

// 配置文件无新 key 时，三个语义搜索标定值维持改配置化之前的硬编码默认值。
func TestSearchCalibrationDefaults(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.Equal(t, 0.0, Cfg.MinMatchSimilarity)
	require.Equal(t, 0.03, Cfg.SimDisplayFloor)
	require.Equal(t, 0.13, Cfg.SimDisplayCeil)
}

// 配置文件显式给出这三个 key 时按配置值生效。
func TestSearchCalibrationOverride(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	sample := "[photos]\nMinMatchSimilarity = 0.2\nSimDisplayFloor = 0.05\nSimDisplayCeil = 0.2\n"
	require.NoError(t, Init(cf, sample))
	require.Equal(t, 0.2, Cfg.MinMatchSimilarity)
	require.Equal(t, 0.05, Cfg.SimDisplayFloor)
	require.Equal(t, 0.2, Cfg.SimDisplayCeil)
}

// 配置文件无 SearchCutAlpha key 时默认 0.7（照 MinMatchSimilarity 的兜底模式）。
func TestSearchCutAlphaDefault(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.Equal(t, 0.7, Cfg.SearchCutAlpha)
}

// 配置文件显式给出 SearchCutAlpha 时按配置值生效。
func TestSearchCutAlphaOverride(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\nSearchCutAlpha = 0.55\n"))
	require.Equal(t, 0.55, Cfg.SearchCutAlpha)
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
