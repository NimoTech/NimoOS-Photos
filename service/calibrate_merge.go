package service

import (
	"database/sql"
	"fmt"
)

// ── T-merge cluster-merge cut-point calibration core ────────────────────────
//
// This is the shared core behind cmd/cluster-analysis's `-mode merge` offline
// report and the in-service calibration runner: it turns the accumulated,
// user-decided cluster-merge questions (merge_suggestions rows with
// status IN ('accepted','rejected'), pkg/sqlite/db.go) into a proposed
// ClusterMergeEps cut point. Mirrors service/calibrate_knn.go's structure
// (truth loading / insufficient-data bars / recommendation, split the same
// way between this shared core and the CLI's report printing).

// MergeMinDecided/MergeMinAccepted/MergeMinRejected/MergeMinPersons are the
// insufficient-data guard bars (spec Sec.4.1, user-approved defaults): below
// any of these, the recommended cut point is not yet trustworthy, though the
// distributions above it are still useful early signal.
const (
	MergeMinDecided  = 30
	MergeMinAccepted = 10
	MergeMinRejected = 5
	MergeMinPersons  = 8
)

// MergeTruth is every usable decided cluster-merge question loaded by
// LoadMergeTruth: the dist each was decided at, split by the user's verdict,
// plus the union of person IDs contributing at least one decided row.
type MergeTruth struct {
	AcceptedDists, RejectedDists []float64
	DistinctPersons              map[string]bool // union of person_a/person_b over decided rows
}

// LoadMergeTruth reads decided merge_suggestions rows:
//
//	SELECT person_a, person_b, dist, status FROM merge_suggestions
//	WHERE status IN ('accepted','rejected')
//
// face_negative_pairs is NOT consumed here (it stores no distance; it stays
// an enforcement-side table) -- explicit revision of spec Sec.4.1's truth-source
// wording, reviewers judge against this plan.
func LoadMergeTruth(db *sql.DB) (MergeTruth, error) {
	rows, err := db.Query(`
		SELECT person_a, person_b, dist, status FROM merge_suggestions
		WHERE status IN ('accepted','rejected')`)
	if err != nil {
		return MergeTruth{}, fmt.Errorf("merge: query decided suggestions: %w", err)
	}
	defer rows.Close()

	t := MergeTruth{DistinctPersons: map[string]bool{}}
	for rows.Next() {
		var personA, personB, status string
		var dist float64
		if err := rows.Scan(&personA, &personB, &dist, &status); err != nil {
			return MergeTruth{}, fmt.Errorf("merge: scan: %w", err)
		}
		switch status {
		case "accepted":
			t.AcceptedDists = append(t.AcceptedDists, dist)
		case "rejected":
			t.RejectedDists = append(t.RejectedDists, dist)
		}
		t.DistinctPersons[personA] = true
		t.DistinctPersons[personB] = true
	}
	if err := rows.Err(); err != nil {
		return MergeTruth{}, fmt.Errorf("merge: rows err: %w", err)
	}
	return t, nil
}

// MergeInsufficient applies the requirement's four bars: total decided
// count, accepted count, rejected count, and distinct-person count.
func MergeInsufficient(t MergeTruth) bool {
	decided := len(t.AcceptedDists) + len(t.RejectedDists)
	return decided < MergeMinDecided ||
		len(t.AcceptedDists) < MergeMinAccepted ||
		len(t.RejectedDists) < MergeMinRejected ||
		len(t.DistinctPersons) < MergeMinPersons
}

// MergeCutPoint proposes a ClusterMergeEps from decided pairs, zero-false-
// accept style: cut = the largest accepted dist strictly below EVERY
// rejected dist (i.e. below min(RejectedDists)). ok=false when RejectedDists
// is empty (unbounded above -- bars prevent this in production, kept
// defensive), when AcceptedDists is empty, or when no accepted dist lies
// strictly below the smallest rejected one.
func MergeCutPoint(t MergeTruth) (cut float64, ok bool) {
	if len(t.RejectedDists) == 0 || len(t.AcceptedDists) == 0 {
		return 0, false
	}
	minRejected := t.RejectedDists[0]
	for _, d := range t.RejectedDists[1:] {
		if d < minRejected {
			minRejected = d
		}
	}

	found := false
	for _, d := range t.AcceptedDists {
		if d < minRejected && (!found || d > cut) {
			cut = d
			found = true
		}
	}
	return cut, found
}
