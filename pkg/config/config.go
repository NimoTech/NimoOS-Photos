package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/spf13/viper"
)

var Cfg *Config

// configPath is the file Init loaded from; Save writes back to the same file.
var configPath = "/etc/nimoos/photos.conf"

type Config struct {
	RuntimePath      string
	LogPath          string
	DataPath         string
	MLEndpoint       string
	Workers          int
	WatchDirs        []string
	RetentionDays    int
	ScanInterval     int
	FacesEnabled     bool
	ScenesEnabled    bool
	OCREnabled       bool
	SmartViewEnabled bool
	// AestheticEnabled controls aesthetic scoring: when off, no inline
	// scoring and no BackfillAesthetic run; existing scores are kept and
	// keep participating in cover ranking, while new assets' NULL scores
	// fall back to each site's own ranking.
	AestheticEnabled bool

	// PreviewPregen controls whether the low-bitrate video hover preview
	// (preview.mp4, can reach tens of MB each) is pre-generated during
	// indexing/startup backfill. Default false = purely lazy generation:
	// only generated on the spot by the route handler (route/v1/assets.go
	// Preview) when the user actually hovers to preview, so disk isn't spent
	// on videos that are never previewed. sprite.jpg (the sprite sheet, a
	// few hundred KB) is unaffected by this toggle and is always pre-generated.
	PreviewPregen bool

	// MinMatchSimilarity, when > 0, drops SmartSearch results whose (display-scale,
	// post-recalibration) match score is below it. Left at 0 (no filtering) by
	// default — see service/search.go for the full rationale and calibration
	// history of this knob.
	MinMatchSimilarity float64
	// MinPersonConfidence is the cohesion floor for exposing UNNAMED
	// auto-clusters via the People list / relations / merge-suggestion APIs.
	// DBSCAN chaining can occasionally weld many different people into one
	// low-cohesion "garbage bin" cluster; gating on cluster confidence keeps
	// it out of every user-facing surface while the data stays in the DB for
	// future re-clustering. Named/favorited/related persons are never gated.
	// Defaults to 0.5 when absent from the config file.
	MinPersonConfidence float64
	// ClusterEpsilon is the DBSCAN cosine-distance threshold for face
	// clustering. An offline study on a real library (OVERVIEW.md "Face
	// clustering parameters") found a percolation cliff at ~0.50: above it,
	// transitive chaining welds unrelated people into one garbage mega-
	// cluster (observed: 59% of all faces in one unnamed person at the
	// legacy 0.60). 0.48 keeps a safe margin below the cliff while
	// preserving named-person purity; under-clustering is recovered by the
	// merge-suggestion band, whose lower bound follows this value.
	// Defaults to 0.48 when absent from the config file.
	ClusterEpsilon float64
	// SimDisplayFloor/SimDisplayCeil linearly rescale the raw CLIP cosine
	// similarity into the [0,1] display score shown to the UI. Defaults
	// (0.03/0.13) were empirically calibrated against the current CLIP model
	// (nllb-clip-large-siglip__v1); see service/scan.go's displayScore for
	// details. Only override these after a fresh empirical pass following a
	// model or re-embed change.
	SimDisplayFloor float64
	SimDisplayCeil  float64

	// SearchCutAlpha is the relative-threshold multiplier used by SmartSearch's
	// belowCut tiering: on the semantic-hit subsequence (sorted descending),
	// the first score below SearchCutAlpha × Top-1 marks (one of the two)
	// candidate cut points into the "more results" tier. Defaults to 0.7 (see
	// service/searchcut.go's semanticCutIndex for the full rule, including the
	// cliff-detection signal it combines with).
	SearchCutAlpha float64

	// Weights and calibration for the doc-classification mixed criterion
	// (OCR class), see service/docscore.go: DocWSem/DocWGeo are the semantic
	// and geometric weights; DocScoreFloor is the weighted-score floor for
	// classifying as a document; DocSemFloor/DocSemCeil are the two
	// calibration endpoints for linearly normalizing the semantic margin.
	// Defaults are empirical values calibrated against the current library;
	// review after a CLIP model generation change.
	DocWSem       float64
	DocWGeo       float64
	DocScoreFloor float64
	DocSemFloor   float64
	DocSemCeil    float64
}

func Init(configFile, confSample string) error {
	if configFile == "" {
		configFile = "/etc/nimoos/photos.conf"
	}
	configPath = configFile
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.WriteFile(configFile, []byte(confSample), 0644); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	v := viper.New()
	v.SetConfigFile(configFile)
	v.SetConfigType("ini")
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}
	watchRaw := v.GetString("photos.WatchDirs")
	var watchDirs []string
	for _, d := range strings.Split(watchRaw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			watchDirs = append(watchDirs, d)
		}
	}
	// watchDirs empty => watcher auto mode (scope = EnumerateScanRoots, i.e.
	// the system disk + all mounted user partitions, dynamically following
	// mounts); non-empty => manual watch list (backward compatible).
	Cfg = &Config{
		RuntimePath:      v.GetString("common.RuntimePath"),
		LogPath:          v.GetString("common.LogPath"),
		DataPath:         v.GetString("photos.DataPath"),
		MLEndpoint:       v.GetString("photos.MLEndpoint"),
		Workers:          v.GetInt("photos.Workers"),
		WatchDirs:        watchDirs,
		RetentionDays:    v.GetInt("photos.RetentionDays"),
		ScanInterval:     v.GetInt("photos.ScanInterval"),
		FacesEnabled:     v.GetBool("photos.FacesEnabled"),
		ScenesEnabled:    v.GetBool("photos.ScenesEnabled"),
		OCREnabled:       v.GetBool("photos.OCREnabled"),
		SmartViewEnabled: v.GetBool("photos.SmartViewEnabled"),
		AestheticEnabled: v.GetBool("photos.AestheticEnabled"),
		PreviewPregen:    v.GetBool("photos.PreviewPregen"),

		MinMatchSimilarity:  v.GetFloat64("photos.MinMatchSimilarity"),
		MinPersonConfidence: v.GetFloat64("photos.MinPersonConfidence"),
		ClusterEpsilon:      v.GetFloat64("photos.ClusterEpsilon"),
		SimDisplayFloor:     v.GetFloat64("photos.SimDisplayFloor"),
		SimDisplayCeil:      v.GetFloat64("photos.SimDisplayCeil"),
		SearchCutAlpha:      v.GetFloat64("photos.SearchCutAlpha"),

		DocWSem:       v.GetFloat64("photos.DocWSem"),
		DocWGeo:       v.GetFloat64("photos.DocWGeo"),
		DocScoreFloor: v.GetFloat64("photos.DocScoreFloor"),
		DocSemFloor:   v.GetFloat64("photos.DocSemFloor"),
		DocSemCeil:    v.GetFloat64("photos.DocSemCeil"),
	}
	if Cfg.RuntimePath == "" {
		Cfg.RuntimePath = "/var/run/nimoos"
	}
	if Cfg.LogPath == "" {
		Cfg.LogPath = "/var/log/nimoos"
	}
	if Cfg.DataPath == "" {
		Cfg.DataPath = "/DATA/.system_data/photos"
	}
	if Cfg.MLEndpoint == "" {
		Cfg.MLEndpoint = common.DefaultMLEndpoint
	}
	if Cfg.Workers == 0 {
		Cfg.Workers = common.DefaultWorkers
	}
	if Cfg.RetentionDays <= 0 {
		Cfg.RetentionDays = 30
	}
	// FacesEnabled defaults to on when absent from the config (no such key).
	if !v.IsSet("photos.FacesEnabled") {
		Cfg.FacesEnabled = true
	}
	// New toggles default to on when absent from the config (no such key), same semantics as FacesEnabled.
	if !v.IsSet("photos.ScenesEnabled") {
		Cfg.ScenesEnabled = true
	}
	if !v.IsSet("photos.OCREnabled") {
		Cfg.OCREnabled = true
	}
	if !v.IsSet("photos.SmartViewEnabled") {
		Cfg.SmartViewEnabled = true
	}
	if !v.IsSet("photos.AestheticEnabled") {
		Cfg.AestheticEnabled = true
	}
	// Scan interval (minutes); defaults to 1440 (24h) when absent from the config. 0 = disable periodic rescans.
	if !v.IsSet("photos.ScanInterval") {
		Cfg.ScanInterval = 1440
	}
	// Semantic search relevance floor: defaults to 0 (no filtering) when absent from the config, matching the hardcoded behavior before this became configurable.
	if !v.IsSet("photos.MinMatchSimilarity") {
		Cfg.MinMatchSimilarity = 0.0
	}
	// Unnamed-cluster confidence floor: defaults to 0.5 when absent from the
	// config, so low-cohesion "garbage bin" clusters stay out of the People
	// list/relations/merge-suggestions by default.
	if !v.IsSet("photos.MinPersonConfidence") {
		Cfg.MinPersonConfidence = 0.5
	}
	// DBSCAN clustering epsilon: defaults to 0.48 when absent from the config,
	// keeping a safe margin below the ~0.50 percolation cliff (see the
	// ClusterEpsilon field doc above).
	if !v.IsSet("photos.ClusterEpsilon") {
		Cfg.ClusterEpsilon = 0.48
	}
	// Display-layer calibration interval endpoints: use the current model's empirical defaults when absent from the config.
	if !v.IsSet("photos.SimDisplayFloor") {
		Cfg.SimDisplayFloor = 0.03
	}
	if !v.IsSet("photos.SimDisplayCeil") {
		Cfg.SimDisplayCeil = 0.13
	}
	// Relative threshold coefficient for the semantic search adaptive cliff: defaults to 0.7 when absent from the config.
	if !v.IsSet("photos.SearchCutAlpha") {
		Cfg.SearchCutAlpha = 0.7
	}
	// The five doc-classification criteria (DocWSem/DocWGeo/DocScoreFloor/
	// DocSemFloor/DocSemCeil) have no fallback here: a 0 value falls back to
	// the empirical default in service/docscore.go's accessor (same pattern as simDisplayFloor).
	return nil
}

// Settings is the subset of Config persisted via the settings API.
type Settings struct {
	WatchDirs        []string
	RetentionDays    int
	ScanInterval     int
	FacesEnabled     bool
	ScenesEnabled    bool
	OCREnabled       bool
	SmartViewEnabled bool
}

// Save writes the settings back to the config file and updates Cfg.
//
// Viper's WriteConfig derives the encoder from the file extension, so files
// ending in ".conf" (not in viper's SupportedExts) would fail.  We work
// around this by writing to a sibling ".ini" temp file and then renaming it
// atomically to the real path.
func Save(s Settings) error {
	if Cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("ini")
	_ = v.ReadInConfig()
	v.Set("photos.WatchDirs", strings.Join(s.WatchDirs, ","))
	if s.RetentionDays > 0 {
		v.Set("photos.RetentionDays", s.RetentionDays)
	}
	v.Set("photos.FacesEnabled", s.FacesEnabled)
	v.Set("photos.ScenesEnabled", s.ScenesEnabled)
	v.Set("photos.OCREnabled", s.OCREnabled)
	v.Set("photos.SmartViewEnabled", s.SmartViewEnabled)
	v.Set("photos.ScanInterval", s.ScanInterval)
	// WriteConfig uses the file extension to choose the encoder; ".conf" is not
	// in viper's SupportedExts.  Write to a ".ini" temp file then rename.
	tmpPath := configPath + ".ini"
	if err := v.WriteConfigAs(tmpPath); err != nil {
		return fmt.Errorf("config.Save: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config.Save rename: %w", err)
	}
	Cfg.WatchDirs = s.WatchDirs
	if s.RetentionDays > 0 {
		Cfg.RetentionDays = s.RetentionDays
	}
	Cfg.FacesEnabled = s.FacesEnabled
	Cfg.ScenesEnabled = s.ScenesEnabled
	Cfg.OCREnabled = s.OCREnabled
	Cfg.SmartViewEnabled = s.SmartViewEnabled
	Cfg.ScanInterval = s.ScanInterval
	return nil
}
