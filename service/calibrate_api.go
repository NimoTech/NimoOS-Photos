package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
)

// ── Threshold self-calibration API (Task 9) ─────────────────────────────
//
// Backs the three HTTP endpoints in route/v1/persons.go
// (GetCalibration/CalibrationHistory/PutCalibrationProfile): read-only
// status/history plus the factory-profile hot-update write path. Deliberately
// thin -- every number here is produced by calibrate_resolve.go/
// calibrate_profile.go's resolver+profile plumbing or calibrate_knn.go/
// calibrate_merge.go/calibrate_twopass.go's truth-loading cores (the same
// ones calibrate_run.go's runner already uses), never a second
// implementation of that logic.

// calibBar is one evidence-bar's progress: how much truth has accumulated
// so far against how much the tier's insufficient-data guard requires.
type calibBar struct {
	Have int `json:"have"`
	Need int `json:"need"`
}

// calibLastRunInfo returns the tier's most recent calibration_history row's
// run_at/outcome (any outcome), or nil/nil when the tier has no history row
// yet. Distinct from calibrate_run.go's calibLastHistoryRunAt (a *FaceService
// method used by the runner's throttle, returning only run_at): this is a
// bare *sql.DB helper because CalibrationStatus builds all three tiers'
// status without needing anything else runner-specific, and it also needs
// the outcome, which the runner's helper doesn't return.
func calibLastRunInfo(db *sql.DB, tier string) (*time.Time, *string, error) {
	var raw sql.NullString
	var outcome string
	err := db.QueryRow(
		`SELECT run_at, outcome FROM calibration_history WHERE tier=? ORDER BY run_at DESC, id DESC LIMIT 1`, tier).
		Scan(&raw, &outcome)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("calibration status: last run for tier %q: %w", tier, err)
	}
	return parseSQLiteTime(raw), &outcome, nil
}

// calibKNNTierStatus builds the "knn" tier's status block: the same
// LoadKNNTruth truth-loading core calibrate_run.go's runKNNTier uses, scored
// against KNNMinPositives/KNNMinNegatives/KNNMinPersons.
func (s *FaceService) calibKNNTierStatus() (map[string]any, error) {
	truth, err := LoadKNNTruth(s.db, assignK())
	if err != nil {
		return nil, fmt.Errorf("calibration status: knn tier: %w", err)
	}
	lastRun, lastOutcome, err := calibLastRunInfo(s.db, "knn")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"tier": "knn",
		"bars": map[string]calibBar{
			"positives": {Have: len(truth.Positives), Need: KNNMinPositives},
			"negatives": {Have: len(truth.Negatives), Need: KNNMinNegatives},
			"persons":   {Have: len(truth.DistinctPersons), Need: KNNMinPersons},
		},
		"lastRun":     lastRun,
		"lastOutcome": lastOutcome,
	}, nil
}

// calibMergeTierStatus builds the "merge" tier's status block: the same
// LoadMergeTruth truth-loading core calibrate_run.go's runMergeTier uses,
// scored against MergeMinDecided/MergeMinAccepted/MergeMinRejected/MergeMinPersons.
func (s *FaceService) calibMergeTierStatus() (map[string]any, error) {
	truth, err := LoadMergeTruth(s.db)
	if err != nil {
		return nil, fmt.Errorf("calibration status: merge tier: %w", err)
	}
	decided := len(truth.AcceptedDists) + len(truth.RejectedDists)
	lastRun, lastOutcome, err := calibLastRunInfo(s.db, "merge")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"tier": "merge",
		"bars": map[string]calibBar{
			"decided":  {Have: decided, Need: MergeMinDecided},
			"accepted": {Have: len(truth.AcceptedDists), Need: MergeMinAccepted},
			"rejected": {Have: len(truth.RejectedDists), Need: MergeMinRejected},
			"persons":  {Have: len(truth.DistinctPersons), Need: MergeMinPersons},
		},
		"lastRun":     lastRun,
		"lastOutcome": lastOutcome,
	}, nil
}

// calibTwoPassTierStatus builds the "twopass" tier's status block. Reuses
// calibLoadNamedTruth (calibrate_run.go), the exact named-person/member-face
// query runTwoPassTier itself loads truth from, rather than re-deriving the
// counts with separate SQL. context.Background() is fine here: the query is
// a plain read against s.db's own WAL snapshot, same as the runner's use
// (which is itself invoked without any request-scoped deadline).
func (s *FaceService) calibTwoPassTierStatus() (map[string]any, error) {
	named, err := s.calibLoadNamedTruth(context.Background())
	if err != nil {
		return nil, fmt.Errorf("calibration status: twopass tier: %w", err)
	}
	namedFaceCount := 0
	for _, np := range named {
		namedFaceCount += len(np.FaceIDs)
	}
	lastRun, lastOutcome, err := calibLastRunInfo(s.db, "twopass")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"tier": "twopass",
		"bars": map[string]calibBar{
			"namedPersons": {Have: len(named), Need: TwoPassMinNamedPersons},
			"namedFaces":   {Have: namedFaceCount, Need: TwoPassMinNamedFaces},
		},
		"lastRun":     lastRun,
		"lastOutcome": lastOutcome,
	}, nil
}

// CalibrationStatus assembles the GET /persons/calibration response body:
// each of the five calibratable thresholds' effective value/source (via the
// exact resolveThreshold layering production's accessors use), the stored
// profile's version, and each tier's evidence-bar progress plus last-run
// metadata.
func (s *FaceService) CalibrationStatus() (map[string]any, error) {
	thresholds := make([]map[string]any, 0, len(calibratableKeys))
	for _, key := range calibratableKeys {
		value, source := resolveThreshold(key, calibCodeDefaults[key])
		thresholds = append(thresholds, map[string]any{
			"key":       key,
			"effective": value,
			"source":    string(source),
			"modelGen":  common.MLModelGen,
		})
	}

	profile := loadCalibrationProfile(s.db)

	knnTier, err := s.calibKNNTierStatus()
	if err != nil {
		return nil, err
	}
	mergeTier, err := s.calibMergeTierStatus()
	if err != nil {
		return nil, err
	}
	twopassTier, err := s.calibTwoPassTierStatus()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"thresholds":     thresholds,
		"profileVersion": profile.Version,
		"tiers":          []map[string]any{knnTier, mergeTier, twopassTier},
	}, nil
}

// CalibrationHistory returns up to limit calibration_history rows
// (run_at DESC), each shaped for the GET /persons/calibration/history
// response's "runs" array. limit<=0 defaults to 50; anything above 500 is
// capped at 500.
func (s *FaceService) CalibrationHistory(limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	rows, err := s.db.Query(`
		SELECT id, run_at, model_gen, tier, truth_counts, old_values, new_values, outcome
		FROM calibration_history ORDER BY run_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("calibration history: query: %w", err)
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var runAtRaw sql.NullString
		var modelGen, tier, truthJSON, oldJSON, newJSON, outcome string
		if err := rows.Scan(&id, &runAtRaw, &modelGen, &tier, &truthJSON, &oldJSON, &newJSON, &outcome); err != nil {
			return nil, fmt.Errorf("calibration history: scan: %w", err)
		}

		var truthCounts map[string]any
		if err := json.Unmarshal([]byte(truthJSON), &truthCounts); err != nil {
			return nil, fmt.Errorf("calibration history: unmarshal truth_counts: %w", err)
		}
		var oldValues, newValues map[string]float64
		if err := json.Unmarshal([]byte(oldJSON), &oldValues); err != nil {
			return nil, fmt.Errorf("calibration history: unmarshal old_values: %w", err)
		}
		if err := json.Unmarshal([]byte(newJSON), &newValues); err != nil {
			return nil, fmt.Errorf("calibration history: unmarshal new_values: %w", err)
		}

		out = append(out, map[string]any{
			"id":          id,
			"runAt":       parseSQLiteTime(runAtRaw),
			"modelGen":    modelGen,
			"tier":        tier,
			"outcome":     outcome,
			"truthCounts": truthCounts,
			"oldValues":   oldValues,
			"newValues":   newValues,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("calibration history: rows: %w", err)
	}
	return out, nil
}

// UpdateCalibrationProfile validates and persists raw as the new stored
// calibration profile (storeCalibrationProfile), then invalidates the
// threshold resolution cache so the next resolveThreshold call (the very
// next GET /persons/calibration included) observes it immediately -- never
// waiting for the next clustering pass. calibProfileWriteMu serializes this
// against any other concurrent PUT (see its doc comment in
// calibrate_profile.go); the cache is only invalidated on success, so a
// rejected update never disturbs an in-flight resolution.
func (s *FaceService) UpdateCalibrationProfile(raw []byte) (int, error) {
	calibProfileWriteMu.Lock()
	defer calibProfileWriteMu.Unlock()

	version, err := storeCalibrationProfile(s.db, raw)
	if err != nil {
		return 0, err
	}
	invalidateThresholdCache()
	return version, nil
}
