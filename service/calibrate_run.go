package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"go.uber.org/zap"
)

// ── In-service self-calibration runner ──────────────────────────────────────
//
// This ties together calibrate_profile.go/calibrate_resolve.go/
// calibrate_rules.go (the shared plumbing) and calibrate_knn.go/
// calibrate_merge.go/calibrate_twopass.go (the three tiers' truth-loading +
// recommendation cores) into the thing that actually runs unattended: fired
// asynchronously after every successful clustering pass, it decides -- per
// tier -- whether enough new evidence has accumulated to re-earn a threshold
// adjustment, and if so, applies one under the profile's safety rails.

// twopassCadenceDays is the minimum interval between two successive twopass
// tier runs (throttle step 1). Unlike knn/merge, twopass has no natural
// "decided rows since last run" signal (named-person ground truth is read
// straight off persons/face_person, not a decision queue), so its throttle
// falls back to a fixed calendar cadence instead.
const twopassCadenceDays = 30

// calibDecidedMinNewRows is the knn/merge tiers' throttle bar (step 1): both
// require at least this many newly decided rows (person_suggestions /
// merge_suggestions, decided_at past the tier's last history run) before
// even attempting a run.
const calibDecidedMinNewRows = 20

// calibCodeDefaults mirrors the calibratable accessors' own fallback
// literals (assignAutoDist's 0.45, assignSuggestDist's 0.60, tightEps's
// 0.35, mergeEps's 0.55, momentGap's 60 minutes) -- the resolveThreshold
// codeDefault argument for each calibratable key, kept in one place since
// every tier below needs to resolve "current effective value" for keys it
// isn't even touching (the invariant check's would-be five-key set).
var calibCodeDefaults = map[string]float64{
	"AssignAutoDist":    0.45,
	"AssignSuggestDist": 0.60,
	"ClusterTightEps":   0.35,
	"ClusterMergeEps":   0.55,
	"MomentGapMinutes":  60,
}

// calibStepRules returns the (maxStep, minDelta) pair boundAdjust uses for
// one calibratable key: MomentGapMinutes is the lone minutes-valued key
// (Rules.MaxStepMinutes/MinDeltaMinutes); every other calibratable key is a
// cosine-distance value (Rules.MaxStepDist/MinDeltaDist).
func calibStepRules(key string, rules CalibrationRules) (maxStep, minDelta float64) {
	if key == "MomentGapMinutes" {
		return rules.MaxStepMinutes, rules.MinDeltaMinutes
	}
	return rules.MaxStepDist, rules.MinDeltaDist
}

// calibBlockedKeys returns the subset of keys that are conf-explicit
// (reusing confValue's own "explicit AND positive" definition, rather than
// duplicating it) -- step 8's per-key skip: a conf-blocked key is never
// adjusted and never written, because resolveThreshold's layer 1 would mask
// it forever anyway.
func calibBlockedKeys(keys []string) map[string]bool {
	blocked := make(map[string]bool, len(keys))
	for _, k := range keys {
		if _, ok := confValue(k); ok {
			blocked[k] = true
		}
	}
	return blocked
}

// maybeCalibrate is the self-calibration entry point, fired async after each
// successful clustering pass (see faces.go's rebuildPersonsWithProgress).
// Single-flight via s.calibrating's CAS. It deliberately does NOT hard-
// exclude a concurrent clustering pass (a plan-level refinement of the
// spec's "mutual exclusion" requirement): WAL gives every read here a
// consistent snapshot, calibration_state is written in one small
// transaction, and any values this run applies are only ever picked up by
// resolveThreshold's cache on the NEXT clustering pass -- so it never
// triggers reclustering itself and there is no feedback loop.
//
// The first-line guard exists purely so every pre-existing RunClustering/
// RunPipeline test -- none of which call SetCalibrationDB -- stays
// bit-identical: with no DB wired (or no config loaded), calibration is a
// guaranteed no-op, checked before anything else runs, including the CAS.
func (s *FaceService) maybeCalibrate(ctx context.Context) {
	if !calibrationDBWired() || config.Cfg == nil {
		return
	}
	if !s.calibrating.CompareAndSwap(false, true) {
		return
	}
	defer s.calibrating.Store(false)

	// Tiers are independent: one tier's error is logged and never stops the
	// others from getting their turn.
	if err := s.runKNNTier(ctx); err != nil {
		zap.L().Warn("calibration: knn tier failed", zap.Error(err))
	}
	if err := s.runMergeTier(ctx); err != nil {
		zap.L().Warn("calibration: merge tier failed", zap.Error(err))
	}
	if err := s.runTwoPassTier(ctx); err != nil {
		zap.L().Warn("calibration: twopass tier failed", zap.Error(err))
	}
}

// ── knn tier ─────────────────────────────────────────────────────────────

// runKNNTier is the knn tier's full execution skeleton (requirement steps
// 1-8): throttle, bars, grid-scan recommendation, skew guard, then the
// shared bounded-adjustment/invariant/write tail in calibApplyTier.
func (s *FaceService) runKNNTier(ctx context.Context) error {
	const tier = "knn"
	keys := []string{"AssignAutoDist", "AssignSuggestDist"}

	blocked := calibBlockedKeys(keys)
	if len(blocked) == len(keys) {
		return nil // step 8: every key conf-blocked -- nothing this tier could ever do.
	}

	due, err := s.calibDecidedThrottleDue(ctx, tier)
	if err != nil {
		return err
	}
	if !due {
		return nil // step 1: silent skip, no history row.
	}

	truth, err := LoadKNNTruth(s.db, assignK())
	if err != nil {
		return fmt.Errorf("calibration knn tier: load truth: %w", err)
	}
	nPositives, nNegatives, nPersons := len(truth.Positives), len(truth.Negatives), len(truth.DistinctPersons)
	truthCounts := map[string]any{"positives": nPositives, "negatives": nNegatives, "persons": nPersons}

	if KNNInsufficient(nPositives, nNegatives, nPersons) {
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	results := KNNGridScan(truth.Positives, truth.Negatives)
	SortKNNResults(results)
	tAuto, tSuggest, ok := SelectKNNCombo(results)
	if !ok {
		truthCounts["note"] = "no zero-false-accept combo in the T_auto x T_suggest grid"
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	// Step 4 (knn only): skew guard over the positives' person distribution.
	positivePersonIDs := make([]string, len(truth.Positives))
	for i, r := range truth.Positives {
		positivePersonIDs[i] = r.PersonID
	}
	if dominantShare(positivePersonIDs) > 0.60 {
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_skewed")
	}

	suggested := map[string]float64{"AssignAutoDist": tAuto, "AssignSuggestDist": tSuggest}
	return s.calibApplyTier(ctx, tier, keys, blocked, suggested, truthCounts)
}

// ── merge tier ───────────────────────────────────────────────────────────

// runMergeTier is the merge tier's full execution skeleton, mirroring
// runKNNTier's shape (no skew guard -- step 4 is knn-only).
func (s *FaceService) runMergeTier(ctx context.Context) error {
	const tier = "merge"
	keys := []string{"ClusterMergeEps"}

	blocked := calibBlockedKeys(keys)
	if len(blocked) == len(keys) {
		return nil
	}

	due, err := s.calibDecidedThrottleDue(ctx, tier)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}

	truth, err := LoadMergeTruth(s.db)
	if err != nil {
		return fmt.Errorf("calibration merge tier: load truth: %w", err)
	}
	truthCounts := map[string]any{
		"accepted": len(truth.AcceptedDists),
		"rejected": len(truth.RejectedDists),
		"persons":  len(truth.DistinctPersons),
	}

	if MergeInsufficient(truth) {
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	cut, ok := MergeCutPoint(truth)
	if !ok {
		truthCounts["note"] = "no accepted dist strictly below the smallest rejected dist"
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	// SHARED KEY: ClusterMergeEps is also written by runTwoPassTier below
	// (see its own SHARED KEY comment there) -- whichever tier runs last
	// within a given maybeCalibrate pass wins. Safe because both tiers
	// always resolve the CURRENT effective value (resolveThreshold) as
	// boundAdjust's starting point and boundAdjust step-limits the move to
	// at most Rules.MaxStepDist per run, so across successive runs the two
	// tiers can only ever converge on ClusterMergeEps, never jump it.
	suggested := map[string]float64{"ClusterMergeEps": cut}
	return s.calibApplyTier(ctx, tier, keys, blocked, suggested, truthCounts)
}

// ── twopass tier ─────────────────────────────────────────────────────────

// runTwoPassTier is the twopass tier's full execution skeleton: calendar
// throttle (no decided-rows signal), named-person bars, the T_tight x
// T_merge x gap grid scan, purity==1.0 selection, then the shared tail.
func (s *FaceService) runTwoPassTier(ctx context.Context) error {
	const tier = "twopass"
	keys := []string{"ClusterTightEps", "MomentGapMinutes", "ClusterMergeEps"}

	blocked := calibBlockedKeys(keys)
	if len(blocked) == len(keys) {
		return nil
	}

	due, err := s.calibTwoPassThrottleDue(ctx)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}

	faces, err := s.calibLoadFaces(ctx)
	if err != nil {
		return fmt.Errorf("calibration twopass tier: load faces: %w", err)
	}
	named, err := s.calibLoadNamedTruth(ctx)
	if err != nil {
		return fmt.Errorf("calibration twopass tier: load named truth: %w", err)
	}

	namedFaceCount := 0
	for _, np := range named {
		namedFaceCount += len(np.FaceIDs)
	}
	truthCounts := map[string]any{
		"faces":        len(faces.vecs),
		"namedPersons": len(named),
		"namedFaces":   namedFaceCount,
	}

	if len(named) < TwoPassMinNamedPersons || namedFaceCount < TwoPassMinNamedFaces {
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	profile := loadCalibrationProfile(s.db)
	tightSpec := profile.Thresholds["ClusterTightEps"]
	mergeSpec := profile.Thresholds["ClusterMergeEps"]

	results, ok := TwoPassGridScan(ctx, faces.vecs, faces.takenAt, faces.indexedAt, faces.idOf, named, tightSpec, mergeSpec)
	if !ok {
		if ctx.Err() != nil {
			// Cancellation (e.g. graceful shutdown), not insufficiency: abort
			// silently, no history row -- writing held_insufficient here
			// would misreport "not enough evidence" for what is actually
			// "the process is shutting down".
			return nil
		}
		truthCounts["note"] = fmt.Sprintf("face count %d exceeds the TwoPassMaxFaces budget (%d)", len(faces.vecs), TwoPassMaxFaces)
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}
	SortTwoPassResults(results)
	combo, ok := SelectTwoPassCombo(results)
	if !ok {
		truthCounts["note"] = "no purity==1.0 combo in the grid"
		return s.writeCalibHeldRow(ctx, tier, keys, truthCounts, "held_insufficient")
	}

	// SHARED KEY: ClusterMergeEps here overlaps with runMergeTier's own
	// ClusterMergeEps write (see its SHARED KEY comment there) -- whichever
	// tier runs last within a given maybeCalibrate pass wins. Safe because
	// both tiers always resolve the CURRENT effective value
	// (resolveThreshold) as boundAdjust's starting point and boundAdjust
	// step-limits the move to at most Rules.MaxStepDist per run, so across
	// successive runs the two tiers can only ever converge on
	// ClusterMergeEps, never jump it.
	suggested := map[string]float64{
		"ClusterTightEps":  combo.TTight,
		"MomentGapMinutes": combo.Gap.Minutes(),
		"ClusterMergeEps":  combo.TMerge,
	}
	return s.calibApplyTier(ctx, tier, keys, blocked, suggested, truthCounts)
}

// calibFaceSet is the twopass tier's face population, loaded fresh (see
// calibLoadFaces) in the exact shape TwoPassGridScan wants.
type calibFaceSet struct {
	vecs      [][]float32
	takenAt   []time.Time
	indexedAt []time.Time
	idOf      map[string]int
}

// calibLoadFaces loads every active (excluded=0) face tied to a live asset --
// the same face population faces.go's loadFacesWithProgress feeds the apple
// engine's free-face moment segmentation from (embeddings + taken_at/
// indexed_at), minus that function's progress-reporting parameters, which
// this runner has no use for. A read-only query against its own snapshot
// (WAL), taken without any coordination with a concurrent clustering pass by
// design -- see maybeCalibrate's doc comment.
func (s *FaceService) calibLoadFaces(ctx context.Context) (calibFaceSet, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fd.id, fd.embedding, a.taken_at, a.indexed_at
		FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`)
	if err != nil {
		return calibFaceSet{}, err
	}
	defer rows.Close()

	out := calibFaceSet{idOf: map[string]int{}}
	for rows.Next() {
		var id string
		var blob []byte
		var takenAtStr, indexedAtStr sql.NullString
		if err := rows.Scan(&id, &blob, &takenAtStr, &indexedAtStr); err != nil {
			return calibFaceSet{}, err
		}
		var takenAt, indexedAt time.Time
		if t := parseSQLiteTime(takenAtStr); t != nil {
			takenAt = *t
		}
		if t := parseSQLiteTime(indexedAtStr); t != nil {
			indexedAt = *t
		}
		out.idOf[id] = len(out.vecs)
		out.vecs = append(out.vecs, sqlite.DeserializeFloat32(blob))
		out.takenAt = append(out.takenAt, takenAt)
		out.indexedAt = append(out.indexedAt, indexedAt)
	}
	return out, rows.Err()
}

// calibLoadNamedTruth loads every named (non-anonymous) person and their
// member face IDs -- the same named-person ground truth
// cmd/cluster-analysis/main.go's loadNamedPersons builds for -mode twopass's
// offline report, reimplemented here (that helper is unexported from a
// different module package, and log.Fatal's on error, which this runner
// must never do).
func (s *FaceService) calibLoadNamedTruth(ctx context.Context) ([]NamedTruth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM persons WHERE name != ''`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]NamedTruth, 0, len(ids))
	for _, id := range ids {
		faceIDs, err := s.calibLoadPersonFaceIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, NamedTruth{PersonID: id, FaceIDs: faceIDs})
	}
	return out, nil
}

func (s *FaceService) calibLoadPersonFaceIDs(ctx context.Context, personID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT face_id FROM face_person WHERE person_id = ?`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out = append(out, fid)
	}
	return out, rows.Err()
}

// ── Throttle (step 1) ────────────────────────────────────────────────────

// calibLastHistoryRunAt returns the tier's most recent calibration_history
// row's run_at (any outcome), or nil when the tier has no history row yet.
func (s *FaceService) calibLastHistoryRunAt(ctx context.Context, tier string) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT run_at FROM calibration_history WHERE tier=? ORDER BY run_at DESC, id DESC LIMIT 1`, tier).
		Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSQLiteTime(raw), nil
}

// calibLastAppliedRunAt returns the tier's most recent outcome='applied'
// calibration_history row's run_at, or nil when none exists (deliberately
// NOT 'clamped' -- the brief's cooldown check is specifically anchored on
// "applied", so a clamped-only history never blocks a subsequent run on
// cooldown grounds).
func (s *FaceService) calibLastAppliedRunAt(ctx context.Context, tier string) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT run_at FROM calibration_history WHERE tier=? AND outcome='applied' ORDER BY run_at DESC, id DESC LIMIT 1`, tier).
		Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSQLiteTime(raw), nil
}

// calibDecidedThrottleDue implements step 1 for the knn/merge tiers: at
// least calibDecidedMinNewRows newly decided rows (person_suggestions for
// knn, merge_suggestions for merge) with decided_at strictly after the
// tier's last history run_at (any outcome; with no history row yet, every
// decided row counts as "new"), AND at least profile.Rules.CooldownHours
// since the tier's last outcome='applied' row (no applied row yet -> no
// cooldown block). Decided timestamps are compared as parsed time.Time
// values, not raw SQL strings, so this is immune to any DATETIME-string
// offset representation mismatch between this file's writes and the
// pre-existing decided_at writers in merge_questions.go/persons.go.
func (s *FaceService) calibDecidedThrottleDue(ctx context.Context, tier string) (bool, error) {
	var query string
	switch tier {
	case "knn":
		query = `SELECT decided_at FROM person_suggestions WHERE decided_at IS NOT NULL`
	case "merge":
		query = `SELECT decided_at FROM merge_suggestions WHERE decided_at IS NOT NULL`
	default:
		return false, fmt.Errorf("calibration: calibDecidedThrottleDue: unknown tier %q", tier)
	}

	lastRunAt, err := s.calibLastHistoryRunAt(ctx, tier)
	if err != nil {
		return false, err
	}

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return false, err
	}
	newDecided := 0
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return false, err
		}
		t := parseSQLiteTime(raw)
		if t == nil {
			continue
		}
		if lastRunAt == nil || t.After(*lastRunAt) {
			newDecided++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()

	if newDecided < calibDecidedMinNewRows {
		return false, nil
	}

	lastApplied, err := s.calibLastAppliedRunAt(ctx, tier)
	if err != nil {
		return false, err
	}
	if lastApplied != nil {
		profile := loadCalibrationProfile(s.db)
		cooldown := time.Duration(profile.Rules.CooldownHours) * time.Hour
		if time.Since(*lastApplied) < cooldown {
			return false, nil
		}
	}
	return true, nil
}

// calibTwoPassThrottleDue implements step 1 for the twopass tier: due
// immediately when the tier has no history row yet (bars still gate it in
// step 2), otherwise due only once twopassCadenceDays have elapsed since the
// tier's last history row (any outcome).
func (s *FaceService) calibTwoPassThrottleDue(ctx context.Context) (bool, error) {
	lastRunAt, err := s.calibLastHistoryRunAt(ctx, "twopass")
	if err != nil {
		return false, err
	}
	if lastRunAt == nil {
		return true, nil
	}
	return time.Since(*lastRunAt) >= time.Duration(twopassCadenceDays)*24*time.Hour, nil
}

// ── History writes ───────────────────────────────────────────────────────

// calibExecer is satisfied by both *sql.DB and *sql.Tx, letting
// insertCalibHistory serve both the held-row path (a single bare statement)
// and calibApplyTier's write path (inside its one transaction).
type calibExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertCalibHistory appends one calibration_history row.
func insertCalibHistory(ctx context.Context, exec calibExecer, tier string, truthCounts map[string]any, oldValues, newValues map[string]float64, outcome string) error {
	truthJSON, err := json.Marshal(truthCounts)
	if err != nil {
		return fmt.Errorf("calibration: marshal truth_counts: %w", err)
	}
	oldJSON, err := json.Marshal(oldValues)
	if err != nil {
		return fmt.Errorf("calibration: marshal old_values: %w", err)
	}
	newJSON, err := json.Marshal(newValues)
	if err != nil {
		return fmt.Errorf("calibration: marshal new_values: %w", err)
	}
	_, err = exec.ExecContext(ctx, `
		INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
		VALUES(?,?,?,?,?,?,?)`,
		time.Now().UTC(), common.MLModelGen, tier, string(truthJSON), string(oldJSON), string(newJSON), outcome)
	return err
}

// calibCurrentValues resolves each key's current effective value via
// resolveThreshold, for building a held row's old/new_values (identical,
// since nothing was applied).
func (s *FaceService) calibCurrentValues(keys []string) map[string]float64 {
	out := make(map[string]float64, len(keys))
	for _, k := range keys {
		v, _ := resolveThreshold(k, calibCodeDefaults[k])
		out[k] = v
	}
	return out
}

// writeCalibHeldRow writes a single "nothing changed" calibration_history
// row (held_insufficient/held_skewed/held_hysteresis/invariant_violation
// paths that never reach calibApplyTier's write tx): old_values and
// new_values are identical, both the tier's current effective values.
func (s *FaceService) writeCalibHeldRow(ctx context.Context, tier string, keys []string, truthCounts map[string]any, outcome string) error {
	current := s.calibCurrentValues(keys)
	return insertCalibHistory(ctx, s.db, tier, truthCounts, current, current, outcome)
}

// ── Shared tail: bounded adjustment, invariants, write (steps 5-7) ──────────

// calibApplyTier is the shared tail every tier's step 3 (and skew guard, for
// knn) funnels into once it has a candidate suggested value per key:
//
//  1. resolve each of allKeys' current effective value (resolveThreshold);
//  2. run boundAdjust for every key NOT in blocked (step 8's per-key skip);
//     a blocked key's old/new value stays identical and it is never written;
//  3. if every non-blocked key held on hysteresis, write one held_hysteresis
//     row and stop (step 5's "ALL held -> held_hysteresis" rule);
//  4. otherwise build the would-be five-calibratable-key effective set
//     (keys this tier didn't touch keep their current value) and check
//     checkCalibrationInvariants; a violation discards the whole tier
//     (invariant_violation row, no state write) (step 6);
//  5. otherwise, in one transaction: UPSERT calibration_state for every
//     applied/clamped key, INSERT one calibration_history row (outcome
//     'clamped' if any key clamped, else 'applied'; old/new_values cover
//     every one of allKeys), commit, then invalidateThresholdCache() (step
//     7).
//
// allKeys is the tier's full, fixed key list (its old/new_values JSON
// always covers every one of them, blocked or held or not). suggested must
// carry an entry for every key in allKeys that is not in blocked.
func (s *FaceService) calibApplyTier(ctx context.Context, tier string, allKeys []string, blocked map[string]bool, suggested map[string]float64, truthCounts map[string]any) error {
	profile := loadCalibrationProfile(s.db)

	oldValues := make(map[string]float64, len(allKeys))
	newValues := make(map[string]float64, len(allKeys))
	for _, k := range allKeys {
		v, _ := resolveThreshold(k, calibCodeDefaults[k])
		oldValues[k] = v
		newValues[k] = v
	}

	var adjustedKeys []string
	anyClamped := false
	anyAdjusted := false
	unblockedCount := 0
	for _, k := range allKeys {
		if blocked[k] {
			continue
		}
		unblockedCount++
		spec, ok := profile.Thresholds[k]
		if !ok {
			continue // defensive: storeCalibrationProfile guarantees every calibratableKey is present.
		}
		maxStep, minDelta := calibStepRules(k, profile.Rules)
		val, outcome := boundAdjust(oldValues[k], suggested[k], spec, maxStep, minDelta)
		switch outcome {
		case adjustApplied:
			newValues[k] = val
			adjustedKeys = append(adjustedKeys, k)
			anyAdjusted = true
		case adjustClamped:
			newValues[k] = val
			adjustedKeys = append(adjustedKeys, k)
			anyAdjusted = true
			anyClamped = true
		case adjustHysteresis:
			// newValues[k] stays at oldValues[k]: no change for this key.
		}
	}

	if unblockedCount == 0 {
		// Defensive only: every call site already skips the tier entirely
		// (calibBlockedKeys covering allKeys) before ever reaching here.
		return nil
	}

	if !anyAdjusted {
		return insertCalibHistory(ctx, s.db, tier, truthCounts, oldValues, oldValues, string(adjustHysteresis))
	}

	wouldBe := make(map[string]float64, len(calibratableKeys))
	for _, k := range calibratableKeys {
		if v, ok := newValues[k]; ok {
			wouldBe[k] = v
		} else {
			v, _ := resolveThreshold(k, calibCodeDefaults[k])
			wouldBe[k] = v
		}
	}
	if err := checkCalibrationInvariants(wouldBe); err != nil {
		zap.L().Info("calibration: invariant violation, tier discarded",
			zap.String("tier", tier), zap.Error(err))
		return insertCalibHistory(ctx, s.db, tier, truthCounts, oldValues, newValues, "invariant_violation")
	}

	outcome := "applied"
	if anyClamped {
		outcome = "clamped"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, k := range adjustedKeys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calibration_state(key, value, model_gen, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, model_gen=excluded.model_gen, updated_at=excluded.updated_at`,
			k, newValues[k], common.MLModelGen, now); err != nil {
			return fmt.Errorf("calibration: upsert calibration_state[%s]: %w", k, err)
		}
	}
	if err := insertCalibHistory(ctx, tx, tier, truthCounts, oldValues, newValues, outcome); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("calibration: commit: %w", err)
	}

	invalidateThresholdCache()
	zap.L().Info("calibration: tier run",
		zap.String("tier", tier),
		zap.String("outcome", outcome),
		zap.Any("old", oldValues),
		zap.Any("new", newValues),
		zap.Any("truth", truthCounts))
	return nil
}
