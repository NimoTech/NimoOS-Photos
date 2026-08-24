package service

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

func testCalibDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestBuiltinFactoryProfile: the compiled-in profile carries version 1 and
// all five calibratable thresholds with the exact Global Constraints values.
func TestBuiltinFactoryProfile(t *testing.T) {
	p := builtinFactoryProfile()
	require.Equal(t, 1, p.Version)
	require.Len(t, p.Thresholds, len(calibratableKeys))
	for _, k := range calibratableKeys {
		_, ok := p.Thresholds[k]
		require.True(t, ok, "missing threshold key %q", k)
	}

	require.Equal(t, ThresholdSpec{Default: 0.45, Min: 0.38, Max: 0.52}, p.Thresholds["AssignAutoDist"])
	require.Equal(t, ThresholdSpec{Default: 0.60, Min: 0.52, Max: 0.68}, p.Thresholds["AssignSuggestDist"])
	require.Equal(t, ThresholdSpec{Default: 0.35, Min: 0.28, Max: 0.42}, p.Thresholds["ClusterTightEps"])
	require.Equal(t, ThresholdSpec{Default: 0.55, Min: 0.48, Max: 0.65}, p.Thresholds["ClusterMergeEps"])
	require.Equal(t, ThresholdSpec{Default: 60, Min: 15, Max: 120}, p.Thresholds["MomentGapMinutes"])

	require.Equal(t, CalibrationRules{
		MaxStepDist:     0.02,
		MinDeltaDist:    0.01,
		MaxStepMinutes:  15,
		MinDeltaMinutes: 10,
		CooldownHours:   24,
	}, p.Rules)
}

// TestStoreLoadRoundTrip: a profile stored via storeCalibrationProfile is
// read back identically via loadCalibrationProfile.
func TestStoreLoadRoundTrip(t *testing.T) {
	db := testCalibDB(t)

	profile := builtinFactoryProfile()
	profile.Version = 2
	raw, err := json.Marshal(profile)
	require.NoError(t, err)

	gotVersion, err := storeCalibrationProfile(db, raw)
	require.NoError(t, err)
	require.Equal(t, 2, gotVersion)

	loaded := loadCalibrationProfile(db)
	require.Equal(t, profile, loaded)
}

// TestStoreRejectsNonMonotonicVersion: builtin counts as version 1 when
// nothing is stored, so a first store at version <= 1 is rejected; and once
// version 2 is stored, storing version 2 or 1 again is rejected too.
func TestStoreRejectsNonMonotonicVersion(t *testing.T) {
	db := testCalibDB(t)

	profile := builtinFactoryProfile()
	profile.Version = 1
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err, "version equal to the implicit builtin version 1 must be rejected")

	profile.Version = 2
	raw, err = json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.NoError(t, err)

	// Now stored version is 2: storing version 2 again, or version 1, must be rejected.
	profile.Version = 2
	raw, _ = json.Marshal(profile)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err, "version equal to the current stored version must be rejected")

	profile.Version = 1
	raw, _ = json.Marshal(profile)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err, "version less than the current stored version must be rejected")

	// version 3 should succeed.
	profile.Version = 3
	raw, _ = json.Marshal(profile)
	v, err := storeCalibrationProfile(db, raw)
	require.NoError(t, err)
	require.Equal(t, 3, v)
}

// TestStoreRejectsBadJSON: malformed JSON is rejected.
func TestStoreRejectsBadJSON(t *testing.T) {
	db := testCalibDB(t)
	_, err := storeCalibrationProfile(db, []byte("{not json"))
	require.Error(t, err)
}

// TestStoreRejectsMissingKey: dropping one of the five calibratable keys is rejected.
func TestStoreRejectsMissingKey(t *testing.T) {
	db := testCalibDB(t)
	profile := builtinFactoryProfile()
	profile.Version = 2
	delete(profile.Thresholds, "AssignAutoDist")
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err)
}

// TestStoreRejectsInvertedRange: Min > Default violates the spec's ordering constraint.
func TestStoreRejectsInvertedRange(t *testing.T) {
	db := testCalibDB(t)
	profile := builtinFactoryProfile()
	profile.Version = 2
	spec := profile.Thresholds["AssignAutoDist"]
	spec.Min = spec.Default + 1
	profile.Thresholds["AssignAutoDist"] = spec
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err)
}

// TestStoreRejectsNonPositiveMin: Min <= 0 is rejected even if Default/Max are consistent.
func TestStoreRejectsNonPositiveMin(t *testing.T) {
	db := testCalibDB(t)
	profile := builtinFactoryProfile()
	profile.Version = 2
	spec := profile.Thresholds["ClusterTightEps"]
	spec.Min = 0
	profile.Thresholds["ClusterTightEps"] = spec
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err)
}

// TestStoreRejectsZeroRulesField: any Rules field <= 0 is rejected.
func TestStoreRejectsZeroRulesField(t *testing.T) {
	db := testCalibDB(t)
	profile := builtinFactoryProfile()
	profile.Version = 2
	profile.Rules.CooldownHours = 0
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	_, err = storeCalibrationProfile(db, raw)
	require.Error(t, err)
}

// TestLoadWithNoStoredRowReturnsBuiltin: an empty photos_meta table falls
// back to the builtin factory profile.
func TestLoadWithNoStoredRowReturnsBuiltin(t *testing.T) {
	db := testCalibDB(t)
	got := loadCalibrationProfile(db)
	require.Equal(t, builtinFactoryProfile(), got)
}

// TestLoadWithCorruptStoredRowReturnsBuiltin: an unparseable photos_meta row
// (should never happen since storeCalibrationProfile validates, but must not
// brick clustering if it does) falls back to the builtin profile.
func TestLoadWithCorruptStoredRowReturnsBuiltin(t *testing.T) {
	db := testCalibDB(t)
	_, err := db.Exec(`INSERT INTO photos_meta(key,value) VALUES(?,?)`, calibrationProfileMetaKey, "{not json")
	require.NoError(t, err)
	got := loadCalibrationProfile(db)
	require.Equal(t, builtinFactoryProfile(), got)
}
