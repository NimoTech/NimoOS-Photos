package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// loadConfigFromINI writes a temp ini file and calls Init, returning the loaded Cfg.
func loadConfigFromINI(t *testing.T, ini string) *Config {
	t.Helper()
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, os.WriteFile(cf, []byte(ini), 0644))
	require.NoError(t, Init(cf, ini))
	return Cfg
}

// TestWatchDirsEmptyMeansAuto: watchdirs unconfigured (or blank) => Cfg.WatchDirs
// is empty, watcher enters auto mode (scope=EnumerateScanRoots); explicit
// config => the manual list is kept as-is.
func TestWatchDirsEmptyMeansAuto(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.Empty(t, cfg.WatchDirs, "unconfigured watchdirs must be empty (auto mode), must not backfill the old three-directory default")

	cfg = loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\nWatchDirs=/DATA/Gallery,/DATA/Media\n")
	require.Equal(t, []string{"/DATA/Gallery", "/DATA/Media"}, cfg.WatchDirs)
}

// When the config file has none of the new keys, the new toggles default to true (same semantics as FacesEnabled).
func TestNewFlagsDefaultTrue(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.True(t, Cfg.ScenesEnabled)
	require.True(t, Cfg.OCREnabled)
	require.True(t, Cfg.SmartViewEnabled)
	require.True(t, Cfg.AestheticEnabled)
}

// After Save writes to disk, re-Init reads back exactly the same values (including false).
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

// When the config file has none of the new keys, the three semantic search calibration values keep the hardcoded defaults from before this became configurable.
func TestSearchCalibrationDefaults(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.Equal(t, 0.0, Cfg.MinMatchSimilarity)
	require.Equal(t, 0.03, Cfg.SimDisplayFloor)
	require.Equal(t, 0.13, Cfg.SimDisplayCeil)
}

// When the config file explicitly sets these three keys, the config values take effect.
func TestSearchCalibrationOverride(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	sample := "[photos]\nMinMatchSimilarity = 0.2\nSimDisplayFloor = 0.05\nSimDisplayCeil = 0.2\n"
	require.NoError(t, Init(cf, sample))
	require.Equal(t, 0.2, Cfg.MinMatchSimilarity)
	require.Equal(t, 0.05, Cfg.SimDisplayFloor)
	require.Equal(t, 0.2, Cfg.SimDisplayCeil)
}

// When the config file has no SearchCutAlpha key, defaults to 0.7 (same fallback pattern as MinMatchSimilarity).
func TestSearchCutAlphaDefault(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\n"))
	require.Equal(t, 0.7, Cfg.SearchCutAlpha)
}

// When the config file explicitly sets SearchCutAlpha, the config value takes effect.
func TestSearchCutAlphaOverride(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "photos.conf")
	require.NoError(t, Init(cf, "[photos]\nSearchCutAlpha = 0.55\n"))
	require.Equal(t, 0.55, Cfg.SearchCutAlpha)
}

// PreviewPregen should default to false (purely lazy generation: only generated on the spot by the route handler when the user hovers).
func TestPreviewPregenDefaultOff(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.False(t, cfg.PreviewPregen, "PreviewPregen should default to false (purely lazy generation)")
}

// Should take effect when the config file explicitly enables it.
func TestPreviewPregenExplicitOn(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\nPreviewPregen = true\n")
	require.True(t, cfg.PreviewPregen, "PreviewPregen=true should take effect")
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
