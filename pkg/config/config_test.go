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

// When the config file has none of the four apple-engine keys, they default
// to "apple"/60/0.35/0.55 (see the ClusterEngine/MomentGapMinutes/
// ClusterTightEps/ClusterMergeEps field docs).
func TestClusterEngineDefaults(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.Equal(t, "apple", cfg.ClusterEngine)
	require.Equal(t, 60, cfg.MomentGapMinutes)
	require.Equal(t, 0.35, cfg.ClusterTightEps)
	require.Equal(t, 0.55, cfg.ClusterMergeEps)
}

// When the config file explicitly sets the four apple-engine keys, the config
// values take effect.
func TestClusterEngineOverride(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\nClusterEngine = dbscan\nMomentGapMinutes = 45\nClusterTightEps = 0.3\nClusterMergeEps = 0.5\n")
	require.Equal(t, "dbscan", cfg.ClusterEngine)
	require.Equal(t, 45, cfg.MomentGapMinutes)
	require.Equal(t, 0.3, cfg.ClusterTightEps)
	require.Equal(t, 0.5, cfg.ClusterMergeEps)
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

// When the config file has none of the exemplar/KNN-assignment keys, they
// default to the empirically-chosen values from the exemplar-assignment SDD
// (ExemplarMaxPerPerson=24, ExemplarMinScore=0.75, ExemplarMinFrontality=0.5,
// ExemplarMinSharpness=0.3, AssignAutoDist=0.45, AssignSuggestDist=0.60,
// AssignKNNK=5, AssignMinVotes=3).
func TestExemplarAssignmentDefaults(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.Equal(t, 24, cfg.ExemplarMaxPerPerson)
	require.Equal(t, 0.75, cfg.ExemplarMinScore)
	require.Equal(t, 0.5, cfg.ExemplarMinFrontality)
	require.Equal(t, 0.3, cfg.ExemplarMinSharpness)
	require.Equal(t, 0.45, cfg.AssignAutoDist)
	require.Equal(t, 0.60, cfg.AssignSuggestDist)
	require.Equal(t, 5, cfg.AssignKNNK)
	require.Equal(t, 3, cfg.AssignMinVotes)
}

// When the config file explicitly sets the eight exemplar/KNN-assignment
// keys, the config values take effect.
func TestExemplarAssignmentOverride(t *testing.T) {
	sample := "[photos]\nDataPath=/tmp/x\n" +
		"ExemplarMaxPerPerson = 40\n" +
		"ExemplarMinScore = 0.8\n" +
		"ExemplarMinFrontality = 0.6\n" +
		"ExemplarMinSharpness = 0.4\n" +
		"AssignAutoDist = 0.4\n" +
		"AssignSuggestDist = 0.55\n" +
		"AssignKNNK = 7\n" +
		"AssignMinVotes = 4\n"
	cfg := loadConfigFromINI(t, sample)
	require.Equal(t, 40, cfg.ExemplarMaxPerPerson)
	require.Equal(t, 0.8, cfg.ExemplarMinScore)
	require.Equal(t, 0.6, cfg.ExemplarMinFrontality)
	require.Equal(t, 0.4, cfg.ExemplarMinSharpness)
	require.Equal(t, 0.4, cfg.AssignAutoDist)
	require.Equal(t, 0.55, cfg.AssignSuggestDist)
	require.Equal(t, 7, cfg.AssignKNNK)
	require.Equal(t, 4, cfg.AssignMinVotes)
}

// When the config file has neither MergeSuggestBand nor MergeSuggestCap, they
// default to 0.06/30 (see the MergeSuggestBand/MergeSuggestCap field docs).
func TestMergeSuggestDefaults(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.Equal(t, 0.06, cfg.MergeSuggestBand)
	require.Equal(t, 30, cfg.MergeSuggestCap)
}

// When the config file explicitly sets MergeSuggestBand/MergeSuggestCap, the
// config values take effect.
func TestMergeSuggestOverride(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\nMergeSuggestBand = 0.1\nMergeSuggestCap = 10\n")
	require.Equal(t, 0.1, cfg.MergeSuggestBand)
	require.Equal(t, 10, cfg.MergeSuggestCap)
}

// When none of the five self-calibratable keys (AssignAutoDist,
// AssignSuggestDist, ClusterTightEps, ClusterMergeEps, MomentGapMinutes) are
// present in the config file, Cfg.Explicit reports false for all of them.
func TestExplicitCalibrationKeysAllAbsent(t *testing.T) {
	cfg := loadConfigFromINI(t, "[photos]\nDataPath=/tmp/x\n")
	require.False(t, cfg.Explicit["AssignAutoDist"])
	require.False(t, cfg.Explicit["AssignSuggestDist"])
	require.False(t, cfg.Explicit["ClusterTightEps"])
	require.False(t, cfg.Explicit["ClusterMergeEps"])
	require.False(t, cfg.Explicit["MomentGapMinutes"])
}

// When the config file explicitly sets a self-calibratable key, Cfg.Explicit
// reports true for exactly that key and false for the other four.
func TestExplicitCalibrationKeysEachSetIndividually(t *testing.T) {
	cases := []string{"AssignAutoDist", "AssignSuggestDist", "ClusterTightEps", "ClusterMergeEps", "MomentGapMinutes"}
	for _, key := range cases {
		ini := "[photos]\nDataPath=/tmp/x\n" + key + " = 5\n"
		cfg := loadConfigFromINI(t, ini)
		for _, other := range cases {
			if other == key {
				require.True(t, cfg.Explicit[other], "expected Explicit[%q]=true when set in config", other)
			} else {
				require.False(t, cfg.Explicit[other], "expected Explicit[%q]=false when not set in config", other)
			}
		}
	}
}
