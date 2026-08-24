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

// calibratableConfigKeys mirrors service.calibratableKeys (kept as a separate
// copy here to avoid an import cycle: service already imports pkg/config).
// Any change to the self-calibratable threshold set must update both.
var calibratableConfigKeys = []string{
	"AssignAutoDist", "AssignSuggestDist",
	"ClusterTightEps", "ClusterMergeEps", "MomentGapMinutes",
}

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

	// ClusterEngine selects the face-clustering algorithm: "apple" (default)
	// runs the two-pass moment-greedy + complete-linkage HAC engine; "dbscan"
	// keeps the legacy single-pass DBSCAN engine (ClusterEpsilon/assignEpsilon/
	// suggestEpsilon). See OVERVIEW.md / the apple-engine SDD for the full
	// rationale. Defaults to "apple" when absent from the config file.
	ClusterEngine string
	// MomentGapMinutes is the time gap (in minutes) used to segment a person's
	// photos into "moments" for the apple engine's pass-1 greedy clustering:
	// consecutive shots more than this far apart start a new moment. Defaults
	// to 60 when absent from the config file.
	MomentGapMinutes int
	// ClusterTightEps is the cosine-distance epsilon for the apple engine's
	// pass-1 greedy within-moment clustering — deliberately tighter than
	// ClusterEpsilon since it only has to separate faces within a single
	// moment, not across the whole library. Defaults to 0.35 when absent from
	// the config file.
	ClusterTightEps float64
	// ClusterMergeEps is the cosine-distance stop threshold for the apple
	// engine's pass-2 complete-linkage HAC merge of pass-1 clusters into
	// persons. Defaults to 0.55 when absent from the config file.
	ClusterMergeEps float64

	// ── Exemplar templates + KNN assignment (replaces the anchored-person
	//    centroid snap; see the exemplar-assignment SDD) ────────────────────
	// ExemplarMaxPerPerson caps how many quality-gated exemplar faces a
	// person keeps for KNN voting. Defaults to 24 when absent from the config file.
	ExemplarMaxPerPerson int
	// ExemplarMinScore/ExemplarMinFrontality/ExemplarMinSharpness are the
	// quality gate a face_detections row must clear to become (or remain) an
	// exemplar: detector score, pose frontality, and blur/sharpness, all in
	// [0,1] (higher = better). Defaults to 0.75/0.5/0.3 when absent from the
	// config file.
	ExemplarMinScore      float64
	ExemplarMinFrontality float64
	ExemplarMinSharpness  float64
	// AssignAutoDist is the KNN median-distance upper bound for auto-
	// accepting a free-floating face onto a person (no human review).
	// Defaults to 0.45 when absent from the config file.
	AssignAutoDist float64
	// AssignSuggestDist is the KNN median-distance upper bound for the
	// "join" suggestion gray zone (AssignAutoDist, AssignSuggestDist]:
	// queued for review rather than auto-accepted or discarded. Defaults to
	// 0.60 when absent from the config file.
	AssignSuggestDist float64
	// AssignKNNK is the number of nearest exemplars (across all persons)
	// consulted when voting on a free-floating face's assignment. Defaults
	// to 5 when absent from the config file.
	AssignKNNK int
	// AssignMinVotes is the minimum number of the K nearest exemplars that
	// must agree on the same person for that person to win the vote.
	// Defaults to 3 when absent from the config file.
	AssignMinVotes int

	// ── Cluster-merge questions (gray-band merge suggestions) ────────────
	// The apple engine's pass-2 HAC (see ClusterMergeEps) intentionally stops
	// short of merging clusters whose complete-linkage distance exceeds
	// ClusterMergeEps, to stay chaining-resistant. Cluster pairs just ABOVE
	// that stop line are natural "almost merged" candidates: MergeSuggestBand
	// widens the window (ClusterMergeEps, ClusterMergeEps+MergeSuggestBand]
	// that a pair's distance must fall into to be surfaced as a
	// merge_suggestions row (see service/merge_questions.go). Defaults to
	// 0.06 when absent from the config file.
	MergeSuggestBand float64
	// MergeSuggestCap caps how many gray-band candidates are kept (closest
	// dist first) per clustering pass, bounding both the write volume into
	// merge_suggestions and the review queue's size. Defaults to 30 when
	// absent from the config file.
	MergeSuggestCap int

	// Explicit records, for each of the five self-calibratable threshold keys
	// (AssignAutoDist, AssignSuggestDist, ClusterTightEps, ClusterMergeEps,
	// MomentGapMinutes), whether the config file explicitly set it. Threshold
	// self-calibration must never override an operator's explicit choice, so
	// it consults this map before adjusting a key; keys not in this set are
	// absent. Populated from viper.IsSet before the default-backfill logic
	// below runs (which would otherwise erase the explicit/default distinction).
	Explicit map[string]bool
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

	// Explicit tracking for the five self-calibratable threshold keys must be
	// read via v.IsSet here, before the default-backfill `if !v.IsSet(...)`
	// blocks further below run — those blocks only assign fallback values
	// onto Cfg's fields, not onto v, so v.IsSet itself stays accurate
	// regardless of order, but keeping this check up front avoids any future
	// backfill logic accidentally shadowing the distinction.
	explicit := make(map[string]bool, len(calibratableConfigKeys))
	for _, key := range calibratableConfigKeys {
		explicit[key] = v.IsSet("photos." + key)
	}

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

		ClusterEngine:    v.GetString("photos.ClusterEngine"),
		MomentGapMinutes: v.GetInt("photos.MomentGapMinutes"),
		ClusterTightEps:  v.GetFloat64("photos.ClusterTightEps"),
		ClusterMergeEps:  v.GetFloat64("photos.ClusterMergeEps"),

		ExemplarMaxPerPerson:  v.GetInt("photos.ExemplarMaxPerPerson"),
		ExemplarMinScore:      v.GetFloat64("photos.ExemplarMinScore"),
		ExemplarMinFrontality: v.GetFloat64("photos.ExemplarMinFrontality"),
		ExemplarMinSharpness:  v.GetFloat64("photos.ExemplarMinSharpness"),
		AssignAutoDist:        v.GetFloat64("photos.AssignAutoDist"),
		AssignSuggestDist:     v.GetFloat64("photos.AssignSuggestDist"),
		AssignKNNK:            v.GetInt("photos.AssignKNNK"),
		AssignMinVotes:        v.GetInt("photos.AssignMinVotes"),

		MergeSuggestBand: v.GetFloat64("photos.MergeSuggestBand"),
		MergeSuggestCap:  v.GetInt("photos.MergeSuggestCap"),

		Explicit: explicit,
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

	// Cluster engine selector: defaults to "apple" when absent from the config.
	if !v.IsSet("photos.ClusterEngine") {
		Cfg.ClusterEngine = "apple"
	}
	// Moment segmentation gap (minutes): defaults to 60 when absent from the config.
	if !v.IsSet("photos.MomentGapMinutes") {
		Cfg.MomentGapMinutes = 60
	}
	// Apple engine pass-1 greedy epsilon: defaults to 0.35 when absent from the config.
	if !v.IsSet("photos.ClusterTightEps") {
		Cfg.ClusterTightEps = 0.35
	}
	// Apple engine pass-2 HAC stop distance: defaults to 0.55 when absent from the config.
	if !v.IsSet("photos.ClusterMergeEps") {
		Cfg.ClusterMergeEps = 0.55
	}
	// Exemplar template cap: defaults to 24 when absent from the config.
	if !v.IsSet("photos.ExemplarMaxPerPerson") {
		Cfg.ExemplarMaxPerPerson = 24
	}
	// Exemplar quality gate (score/frontality/sharpness): defaults to
	// 0.75/0.5/0.3 when absent from the config.
	if !v.IsSet("photos.ExemplarMinScore") {
		Cfg.ExemplarMinScore = 0.75
	}
	if !v.IsSet("photos.ExemplarMinFrontality") {
		Cfg.ExemplarMinFrontality = 0.5
	}
	if !v.IsSet("photos.ExemplarMinSharpness") {
		Cfg.ExemplarMinSharpness = 0.3
	}
	// KNN assignment dual thresholds: defaults to 0.45/0.60 when absent from the config.
	if !v.IsSet("photos.AssignAutoDist") {
		Cfg.AssignAutoDist = 0.45
	}
	if !v.IsSet("photos.AssignSuggestDist") {
		Cfg.AssignSuggestDist = 0.60
	}
	// KNN neighborhood size and minimum agreeing-vote count: defaults to 5/3 when absent from the config.
	if !v.IsSet("photos.AssignKNNK") {
		Cfg.AssignKNNK = 5
	}
	if !v.IsSet("photos.AssignMinVotes") {
		Cfg.AssignMinVotes = 3
	}
	// Cluster-merge-question gray band: defaults to 0.06 (width above
	// ClusterMergeEps) / 30 (max candidates per pass) when absent from the config.
	if !v.IsSet("photos.MergeSuggestBand") {
		Cfg.MergeSuggestBand = 0.06
	}
	if !v.IsSet("photos.MergeSuggestCap") {
		Cfg.MergeSuggestCap = 30
	}
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
