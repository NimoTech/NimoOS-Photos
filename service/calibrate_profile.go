package service

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// calibProfileWriteMu serializes storeCalibrationProfile calls. Its
// read-current-version-then-insert sequence is not transaction-wrapped, so
// two concurrent writers could both read the same "current version" and
// both pass the monotonicity guard, racing the last INSERT ... ON CONFLICT
// to silently win. In production storeCalibrationProfile has exactly one
// caller (UpdateCalibrationProfile, behind the PUT /persons/calibration/profile
// handler), so this cheap package-level mutex -- rather than a real
// transaction -- is enough to make concurrent PUTs serialize instead of race.
var calibProfileWriteMu sync.Mutex

// calibratableKeys is the closed set of self-calibratable threshold names,
// spelled exactly as their pkg/config field names.
var calibratableKeys = []string{
	"AssignAutoDist", "AssignSuggestDist",
	"ClusterTightEps", "ClusterMergeEps", "MomentGapMinutes",
}

// ThresholdSpec is the calibratable range for a single threshold: the
// factory/current default plus the [Min, Max] band self-calibration is
// allowed to move it within.
type ThresholdSpec struct {
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
}

// CalibrationRules bounds how aggressively self-calibration may move
// thresholds between runs: per-tier max/min step sizes and the cooldown
// between successive applied adjustments.
type CalibrationRules struct {
	MaxStepDist     float64 `json:"maxStepDist"`
	MinDeltaDist    float64 `json:"minDeltaDist"`
	MaxStepMinutes  float64 `json:"maxStepMinutes"`
	MinDeltaMinutes float64 `json:"minDeltaMinutes"`
	CooldownHours   int     `json:"cooldownHours"`
}

// CalibrationProfile is the versioned factory/override document that bounds
// self-calibration: the allowed [Min, Max] band per calibratable threshold,
// plus the step/cooldown rules shared across all of them.
type CalibrationProfile struct {
	Version           int                      `json:"version"`
	MinServiceVersion string                   `json:"minServiceVersion,omitempty"`
	Thresholds        map[string]ThresholdSpec `json:"thresholds"`
	Rules             CalibrationRules         `json:"rules"`
}

// calibrationProfileMetaKey is the photos_meta row key the stored profile is
// persisted under.
const calibrationProfileMetaKey = "calibration_profile"

// builtinFactoryProfile returns the compiled-in version-1 profile (Global
// Constraints table verbatim). It is the fallback used whenever no valid
// override is stored in photos_meta, and also the implicit "version 1"
// baseline that storeCalibrationProfile's monotonicity guard compares
// against when nothing has been stored yet.
func builtinFactoryProfile() CalibrationProfile {
	return CalibrationProfile{
		Version: 1,
		Thresholds: map[string]ThresholdSpec{
			"AssignAutoDist":    {Default: 0.45, Min: 0.38, Max: 0.52},
			"AssignSuggestDist": {Default: 0.60, Min: 0.52, Max: 0.68},
			"ClusterTightEps":   {Default: 0.35, Min: 0.28, Max: 0.42},
			"ClusterMergeEps":   {Default: 0.55, Min: 0.48, Max: 0.65},
			"MomentGapMinutes":  {Default: 60, Min: 15, Max: 120},
		},
		Rules: CalibrationRules{
			MaxStepDist:     0.02,
			MinDeltaDist:    0.01,
			MaxStepMinutes:  15,
			MinDeltaMinutes: 10,
			CooldownHours:   24,
		},
	}
}

// loadCalibrationProfile returns the stored profile from photos_meta, or the
// builtin one when absent/unparseable (unparseable also logs a zap warning:
// a stored profile should never be invalid because storeCalibrationProfile
// validates, but a corrupt row must not brick clustering).
func loadCalibrationProfile(db *sql.DB) CalibrationProfile {
	var raw string
	err := db.QueryRow(`SELECT value FROM photos_meta WHERE key=?`, calibrationProfileMetaKey).Scan(&raw)
	if err != nil {
		return builtinFactoryProfile()
	}
	var profile CalibrationProfile
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		zap.L().Warn("calibration: stored profile is corrupt, falling back to builtin", zap.Error(err))
		return builtinFactoryProfile()
	}
	return profile
}

// storeCalibrationProfile validates raw and persists it. Rejections:
// bad JSON; version <= current stored version (monotonic guard; builtin
// counts as version 1 when nothing stored); any of the five calibratable
// keys missing; any spec violating Min <= Default <= Max or Min <= 0;
// any Rules field <= 0. Returns the accepted version.
func storeCalibrationProfile(db *sql.DB, raw []byte) (int, error) {
	var profile CalibrationProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return 0, fmt.Errorf("calibration profile: invalid JSON: %w", err)
	}

	currentVersion := builtinFactoryProfile().Version
	var storedRaw string
	if err := db.QueryRow(`SELECT value FROM photos_meta WHERE key=?`, calibrationProfileMetaKey).Scan(&storedRaw); err == nil {
		var stored CalibrationProfile
		if err := json.Unmarshal([]byte(storedRaw), &stored); err == nil {
			currentVersion = stored.Version
		}
	}
	if profile.Version <= currentVersion {
		return 0, fmt.Errorf("calibration profile: version %d is not greater than current version %d", profile.Version, currentVersion)
	}

	for _, key := range calibratableKeys {
		spec, ok := profile.Thresholds[key]
		if !ok {
			return 0, fmt.Errorf("calibration profile: missing threshold %q", key)
		}
		if spec.Min <= 0 {
			return 0, fmt.Errorf("calibration profile: threshold %q has non-positive Min %v", key, spec.Min)
		}
		if !(spec.Min <= spec.Default && spec.Default <= spec.Max) {
			return 0, fmt.Errorf("calibration profile: threshold %q violates Min<=Default<=Max (min=%v default=%v max=%v)", key, spec.Min, spec.Default, spec.Max)
		}
	}

	if profile.Rules.MaxStepDist <= 0 {
		return 0, fmt.Errorf("calibration profile: Rules.MaxStepDist must be > 0")
	}
	if profile.Rules.MinDeltaDist <= 0 {
		return 0, fmt.Errorf("calibration profile: Rules.MinDeltaDist must be > 0")
	}
	if profile.Rules.MaxStepMinutes <= 0 {
		return 0, fmt.Errorf("calibration profile: Rules.MaxStepMinutes must be > 0")
	}
	if profile.Rules.MinDeltaMinutes <= 0 {
		return 0, fmt.Errorf("calibration profile: Rules.MinDeltaMinutes must be > 0")
	}
	if profile.Rules.CooldownHours <= 0 {
		return 0, fmt.Errorf("calibration profile: Rules.CooldownHours must be > 0")
	}

	if _, err := db.Exec(`INSERT INTO photos_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		calibrationProfileMetaKey, string(raw)); err != nil {
		return 0, fmt.Errorf("calibration profile: write failed: %w", err)
	}
	return profile.Version, nil
}
