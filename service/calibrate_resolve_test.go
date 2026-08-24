package service

import (
	"encoding/json"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// resetCalibState clears both the resolver's wired DB (and its cache) and
// config.Cfg, restoring the pre-test globals on cleanup. Every test in this
// file must call this first: resolveThreshold/confValue read package-level
// state shared across the whole `service` test binary.
func resetCalibState(t *testing.T) {
	t.Helper()
	prevCfg := config.Cfg
	t.Cleanup(func() {
		config.Cfg = prevCfg
		SetCalibrationDB(nil)
	})
	config.Cfg = nil
	SetCalibrationDB(nil)
}

// TDD case 1: no DB wired, no conf loaded -> falls all the way through to
// the code default (today's pre-calibration accessor behavior).
func TestResolveThreshold_NoDBNoConf_CodeDefault(t *testing.T) {
	resetCalibState(t)

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.45, v)
	require.Equal(t, sourceCode, src)
}

// TDD case 2: an explicit, positive conf value wins even when a matching
// calibration_state row also exists.
func TestResolveThreshold_ConfExplicitOverridesEverything(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	_, err := db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at)
		VALUES('AssignAutoDist',0.50,?,CURRENT_TIMESTAMP)`, common.MLModelGen)
	require.NoError(t, err)
	config.Cfg = &config.Config{
		AssignAutoDist: 0.42,
		Explicit:       map[string]bool{"AssignAutoDist": true},
	}

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.42, v)
	require.Equal(t, sourceConf, src)
}

// TDD case 3: a calibration_state row whose model_gen matches
// common.MLModelGen is used.
func TestResolveThreshold_StateRowMatchingModelGen(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	_, err := db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at)
		VALUES('AssignAutoDist',0.50,?,CURRENT_TIMESTAMP)`, common.MLModelGen)
	require.NoError(t, err)

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.50, v)
	require.Equal(t, sourceCalibrated, src)
}

// TDD case 4: a calibration_state row whose model_gen does NOT match is
// skipped, falling through to the stored/builtin profile's default.
func TestResolveThreshold_StateRowWrongModelGenFallsBackToProfile(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	_, err := db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at)
		VALUES('AssignAutoDist',0.50,'stale-gen',CURRENT_TIMESTAMP)`)
	require.NoError(t, err)

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.45, v) // builtin factory profile default for this key
	require.Equal(t, sourceProfile, src)
}

// TDD case 5: a stored photos_meta profile with a changed default, and no
// calibration_state row, resolves to the profile's default.
func TestResolveThreshold_ProfileDefaultWhenNoStateRow(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)

	profile := builtinFactoryProfile()
	profile.Version = 2
	profile.Thresholds["AssignAutoDist"] = ThresholdSpec{Default: 0.48, Min: 0.38, Max: 0.52}
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.NoError(t, err)

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.48, v)
	require.Equal(t, sourceProfile, src)
}

// TDD case 6: resolutions are cached until invalidateThresholdCache is
// called, even if the underlying calibration_state row changes meanwhile.
func TestResolveThreshold_CacheUntilInvalidate(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	_, err := db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at)
		VALUES('AssignAutoDist',0.50,?,CURRENT_TIMESTAMP)`, common.MLModelGen)
	require.NoError(t, err)

	v1, _ := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.50, v1)

	_, err = db.Exec(`UPDATE calibration_state SET value=0.51 WHERE key='AssignAutoDist'`)
	require.NoError(t, err)

	v2, _ := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.50, v2, "cached resolution must not observe the row update")

	invalidateThresholdCache()

	v3, _ := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.51, v3, "after invalidation the new row value must be read")
}

// TDD case 7: an explicit conf value of exactly 0 is treated as not
// explicit, matching the pre-calibration accessors' non-positive fallback
// semantics (e.g. `config.Cfg.AssignAutoDist > 0`).
func TestResolveThreshold_ConfExplicitZeroTreatedAsNotExplicit(t *testing.T) {
	resetCalibState(t)
	config.Cfg = &config.Config{
		AssignAutoDist: 0,
		Explicit:       map[string]bool{"AssignAutoDist": true},
	}

	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.Equal(t, 0.45, v)
	require.Equal(t, sourceCode, src)
}

// confValue must convert the int-typed MomentGapMinutes config field to
// float64 rather than mishandling its type via a type switch/assertion.
func TestConfValue_MomentGapMinutesIntConversion(t *testing.T) {
	resetCalibState(t)
	config.Cfg = &config.Config{
		MomentGapMinutes: 45,
		Explicit:         map[string]bool{"MomentGapMinutes": true},
	}

	v, ok := confValue("MomentGapMinutes")
	require.True(t, ok)
	require.Equal(t, 45.0, v)
}
