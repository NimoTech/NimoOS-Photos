package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
