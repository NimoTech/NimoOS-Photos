package service

import (
	"database/sql"
	"sync"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
)

// thresholdSource records which of the four resolution layers produced a
// resolveThreshold result; currently only consumed by tests, but returned
// alongside the value so future diagnostics (e.g. an admin "why is this
// threshold what it is" endpoint) don't need a second lookup path.
type thresholdSource string

const (
	sourceConf       thresholdSource = "conf"
	sourceCalibrated thresholdSource = "calibrated"
	sourceProfile    thresholdSource = "profile"
	sourceCode       thresholdSource = "code"
)

// resolvedThreshold is one cached resolveThreshold result.
type resolvedThreshold struct {
	value  float64
	source thresholdSource
}

var (
	calibMu        sync.RWMutex
	calibrationDB  *sql.DB
	thresholdCache = map[string]resolvedThreshold{}
)

// SetCalibrationDB wires the resolver to the live DB (called once from
// NewService) and clears the cache. Tests constructing services directly
// leave it nil: resolution then degrades to conf/code exactly like the
// pre-calibration accessors.
func SetCalibrationDB(db *sql.DB) {
	calibMu.Lock()
	defer calibMu.Unlock()
	calibrationDB = db
	thresholdCache = map[string]resolvedThreshold{}
}

// invalidateThresholdCache drops all cached resolutions. Called by the
// calibration runner after writing calibration_state and by
// storeCalibrationProfile's caller after a profile update.
func invalidateThresholdCache() {
	calibMu.Lock()
	defer calibMu.Unlock()
	thresholdCache = map[string]resolvedThreshold{}
}

// calibrationDBWired reports whether SetCalibrationDB has wired a live DB.
// maybeCalibrate's first-line guard (calibrate_run.go) uses this: every
// FaceService built directly by a test (the vast majority of service/*_test.go)
// never calls SetCalibrationDB, so calibrationDB stays nil and the whole
// self-calibration runner is a guaranteed no-op for them -- existing
// RunClustering assertions stay bit-identical despite the new async call.
func calibrationDBWired() bool {
	calibMu.RLock()
	defer calibMu.RUnlock()
	return calibrationDB != nil
}

// confValue reads the raw config.Cfg field for one of the five
// calibratable keys, reporting ok=false when config isn't initialized, the
// config file didn't explicitly set the key, or the explicit value is
// non-positive (treated as "not explicit", matching the pre-calibration
// accessors' fallback semantics). MomentGapMinutes is an int config field
// and is converted to float64 here.
func confValue(key string) (float64, bool) {
	if config.Cfg == nil || !config.Cfg.Explicit[key] {
		return 0, false
	}
	var v float64
	switch key {
	case "AssignAutoDist":
		v = config.Cfg.AssignAutoDist
	case "AssignSuggestDist":
		v = config.Cfg.AssignSuggestDist
	case "ClusterTightEps":
		v = config.Cfg.ClusterTightEps
	case "ClusterMergeEps":
		v = config.Cfg.ClusterMergeEps
	case "MomentGapMinutes":
		v = float64(config.Cfg.MomentGapMinutes)
	default:
		return 0, false
	}
	if v <= 0 {
		return 0, false
	}
	return v, true
}

// resolveThreshold returns the effective value for one calibratable key,
// resolved through four layers in order:
//  1. conf-explicit (config.Cfg.Explicit[key] && value > 0) -> that value
//  2. calibration_state row with model_gen == common.MLModelGen
//  3. loadCalibrationProfile(db).Thresholds[key].Default
//  4. codeDefault
//
// Layers 2-3 are skipped when no DB is wired (SetCalibrationDB never called,
// or called with nil).
//
// Only layers 2-4 are cached (until invalidateThresholdCache clears them);
// layer 1 is re-checked on every call, uncached. This is deliberate, not
// just an optimization: config.Cfg is a process-lifetime constant in
// production, but the test suite swaps it in and out per test via a
// package-level variable, and those tests expect every accessor call to
// observe the config.Cfg in effect right now -- caching the conf layer
// would let a stale resolution from an earlier test leak into a later one.
// Layers 2-4 only depend on the DB, which is stable once wired, so caching
// them is safe and avoids a SQL round trip per accessor call.
func resolveThreshold(key string, codeDefault float64) (float64, thresholdSource) {
	if v, ok := confValue(key); ok {
		return v, sourceConf
	}

	calibMu.RLock()
	if r, ok := thresholdCache[key]; ok {
		calibMu.RUnlock()
		return r.value, r.source
	}
	db := calibrationDB
	calibMu.RUnlock()

	value, source := resolveBelowConf(key, codeDefault, db)

	calibMu.Lock()
	thresholdCache[key] = resolvedThreshold{value: value, source: source}
	calibMu.Unlock()

	return value, source
}

// resolveBelowConf implements layers 2-4 (everything below the conf-explicit
// layer), without touching the cache.
func resolveBelowConf(key string, codeDefault float64, db *sql.DB) (float64, thresholdSource) {
	if db == nil {
		return codeDefault, sourceCode
	}

	var stateValue float64
	var modelGen string
	err := db.QueryRow(`SELECT value, model_gen FROM calibration_state WHERE key=?`, key).Scan(&stateValue, &modelGen)
	if err == nil && modelGen == common.MLModelGen {
		return stateValue, sourceCalibrated
	}

	profile := loadCalibrationProfile(db)
	if spec, ok := profile.Thresholds[key]; ok {
		return spec.Default, sourceProfile
	}
	return codeDefault, sourceCode
}
