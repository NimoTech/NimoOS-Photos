package service

import (
	"database/sql"
	"fmt"
	"math"
	"sort"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// ── KNN exemplar-assignment threshold calibration core ──────────────────────
//
// This is the shared core behind cmd/cluster-analysis's `-mode knn` offline
// report and the in-service calibration runner: it turns accumulated ground
// truth -- face_person rows with confirmed=1 (user-confirmed: this face IS
// this person) and person_negatives rows (user-rejected: this face is NOT
// this person) -- into AssignAutoDist/AssignSuggestDist candidates for the
// KNN exemplar matcher (matcher.go, persons.go's assignAutoDist/
// assignSuggestDist/assignK). Moved verbatim from cmd/cluster-analysis/knn.go
// (report printing / -mode knn's flag wiring stay in the CLI).

// knnTAutoLo/Hi/Step and knnTSugMargin/Hi/Step define the T_auto x
// T_suggest grid: T_auto in [0.35,0.55] step 0.01, T_suggest in
// [T_auto+0.05, 0.70] step 0.01 -- T_suggest must clear T_auto by at least
// this margin for the auto/gray/miss zones below to be well-formed (a
// T_suggest <= T_auto would make the gray zone empty or inverted).
const (
	knnTAutoLo    = 0.35
	knnTAutoHi    = 0.55
	knnTAutoStep  = 0.01
	knnTSugMargin = 0.05
	knnTSugHi     = 0.70
	knnTSugStep   = 0.01
)

// KNNMinPositives/KNNMinNegatives/KNNMinPersons are the insufficient-data
// guard bars: below any of these, the grid-scan recommendation is not yet
// trustworthy, though the distributions above it are still useful early
// signal.
const (
	KNNMinPositives = 100
	KNNMinNegatives = 20
	KNNMinPersons   = 5
)

// knnExemplarFace is one exemplar template face for the solo per-person KNN
// index: the face_person.exemplar=1 row's face ID (needed for self-exclusion
// when a truth row's own face happens to also be one of its person's
// exemplars) plus its embedding.
type knnExemplarFace struct {
	FaceID string
	Vec    []float32
}

// KNNTruthRow is one ground-truth (face, person) pair with its computed KNN
// distance statistic.
type KNNTruthRow struct {
	FaceID   string
	PersonID string
	Dist     float64
}

// KNNTruthSet is every usable truth row loaded by LoadKNNTruth, plus skip
// counters kept for report transparency (a row is skipped, never silently
// dropped without a trace).
type KNNTruthSet struct {
	Positives []KNNTruthRow // confirmed=1 face_person rows (face IS this person)
	Negatives []KNNTruthRow // person_negatives rows (face is NOT this person)

	PosSkippedNoFace     int // face_id not among the loaded (excluded=0, live-asset) faces
	PosSkippedNoExemplar int // person had zero usable exemplars for this face (after self-exclusion)
	NegSkippedNoFace     int
	NegSkippedNoExemplar int

	// DistinctPersons is the set of person IDs contributing at least one
	// USABLE row above -- i.e. the persons the recommendation below is
	// actually built from, not merely every person_id that appears
	// somewhere in the raw truth tables (a person with zero usable
	// exemplars contributes nothing to the recommendation and would be a
	// false signal of sufficiency if counted here).
	DistinctPersons map[string]bool

	// ExemplarPersons/ExemplarFaces count the loaded exemplar templates
	// (face_person.exemplar=1 rows whose face is among the loaded
	// excluded=0/live-asset faces), grouped by person -- report-transparency
	// counts, not consumed by the recommendation itself.
	ExemplarPersons int
	ExemplarFaces   int
}

// knnDistance computes the SAME statistic matcher.go's Match uses for a
// free face against one person: the median distance of that person's
// exemplars among the k nearest of them to vec. It does not reimplement
// matcher.go -- BuildExemplarIndex and Match are both exported, so this
// builds the same single-person solo index production uses for revalidation
// (see faces.go's soloIndex) and calls the real Match.
//
// Match() always zeroes its returned dist when decision=="none" (the caller
// never needs a real distance for a non-match in production), but this tool
// wants the raw distance for EVERY truth row, including the ones that would
// miss the suggest bar entirely -- those are exactly the interesting tail of
// the distributions below. Passing autoDist=suggestDist=+Inf sidesteps the
// zeroing without touching matcher.go: Match's first branch is
// `med <= autoDist`, which a finite med always satisfies against +Inf, so
// decision is always "auto" and dist is always the real median. minVotes=1
// makes the vote-floor moot (irrelevant with exactly one candidate person),
// and the plurality-vote/tie logic degenerates harmlessly to "the lone
// candidate wins" -- see faces.go's soloIndex comment for the same
// single-person-index reasoning.
//
// selfFaceID is excluded from the person's exemplar pool before building
// the index: a confirmed face that also happens to be one of its own
// person's exemplars would otherwise report a trivial (and misleading)
// zero distance against itself.
func knnDistance(exemplars []knnExemplarFace, selfFaceID string, vec []float32, personID string, k int) (dist float64, ok bool) {
	vecs := make([][]float32, 0, len(exemplars))
	for _, e := range exemplars {
		if e.FaceID == selfFaceID {
			continue
		}
		vecs = append(vecs, e.Vec)
	}
	if len(vecs) == 0 {
		return 0, false
	}

	ix := BuildExemplarIndex(map[string][][]float32{personID: vecs})
	winner, med, decision := ix.Match(vec, selfFaceID, nil, k, 1, math.Inf(1), math.Inf(1))
	if decision != "auto" || winner != personID {
		// Should not happen with a non-empty pool and minVotes=1 -- defensive,
		// not a normal code path.
		return 0, false
	}
	return med, true
}

// facePersonPair is one (face_id, person_id) row, shared shape for both the
// confirmed-positive and person_negatives queries below.
type facePersonPair struct {
	FaceID, PersonID string
}

func queryFacePersonPairs(db *sql.DB, query string) ([]facePersonPair, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("knn: query %q: %w", query, err)
	}
	defer rows.Close()
	var out []facePersonPair
	for rows.Next() {
		var p facePersonPair
		if err := rows.Scan(&p.FaceID, &p.PersonID); err != nil {
			return nil, fmt.Errorf("knn: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("knn: rows err: %w", err)
	}
	return out, nil
}

// loadKNNFaceVectors loads face embeddings under the exact same filter the
// offline analysis tool's loadFaces uses (excluded=0, joined to a live --
// i.e. still-existing -- asset row): only the id and embedding are needed
// here, unlike loadFaces' richer Face struct which also carries bbox/exif/
// timestamps for that tool's other analysis modes.
func loadKNNFaceVectors(db *sql.DB) (map[string][]float32, error) {
	rows, err := db.Query(`
		SELECT fd.id, fd.embedding
		FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`)
	if err != nil {
		return nil, fmt.Errorf("knn: load faces: %w", err)
	}
	defer rows.Close()

	out := map[string][]float32{}
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("knn: scan face: %w", err)
		}
		out[id] = sqlite.DeserializeFloat32(blob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("knn: rows err: %w", err)
	}
	return out, nil
}

// loadKNNExemplars reads the persisted exemplar flags directly (face_person
// rows with exemplar=1) -- per the requirement, this does NOT recompute
// exemplars via SelectExemplars; production's exemplar column is the ground
// truth for what the live matcher is actually voting against.
func loadKNNExemplars(db *sql.DB, vecByFace map[string][]float32) (map[string][]knnExemplarFace, error) {
	pairs, err := queryFacePersonPairs(db, `SELECT face_id, person_id FROM face_person WHERE exemplar = 1`)
	if err != nil {
		return nil, err
	}
	out := map[string][]knnExemplarFace{}
	for _, p := range pairs {
		vec, ok := vecByFace[p.FaceID]
		if !ok {
			continue // exemplar face not among loaded (excluded=0, live-asset) faces
		}
		out[p.PersonID] = append(out[p.PersonID], knnExemplarFace{FaceID: p.FaceID, Vec: vec})
	}
	return out, nil
}

// LoadKNNTruth loads faces (excluded=0, joined to a live asset row),
// exemplar templates (face_person.exemplar=1), confirmed positives
// (face_person.confirmed=1) and rejected negatives (person_negatives),
// computing each usable row's KNN median distance via knnDistance (the real
// BuildExemplarIndex/Match solo-index path). k is the caller's AssignKNNK.
func LoadKNNTruth(db *sql.DB, k int) (KNNTruthSet, error) {
	vecByFace, err := loadKNNFaceVectors(db)
	if err != nil {
		return KNNTruthSet{}, err
	}
	exemplarsByPerson, err := loadKNNExemplars(db, vecByFace)
	if err != nil {
		return KNNTruthSet{}, err
	}

	ts := KNNTruthSet{DistinctPersons: map[string]bool{}}
	ts.ExemplarPersons = len(exemplarsByPerson)
	for _, v := range exemplarsByPerson {
		ts.ExemplarFaces += len(v)
	}

	positives, err := queryFacePersonPairs(db, `SELECT face_id, person_id FROM face_person WHERE confirmed = 1 AND person_id IS NOT NULL`)
	if err != nil {
		return KNNTruthSet{}, err
	}
	for _, p := range positives {
		vec, ok := vecByFace[p.FaceID]
		if !ok {
			ts.PosSkippedNoFace++
			continue
		}
		dist, ok := knnDistance(exemplarsByPerson[p.PersonID], p.FaceID, vec, p.PersonID, k)
		if !ok {
			ts.PosSkippedNoExemplar++
			continue
		}
		ts.Positives = append(ts.Positives, KNNTruthRow{FaceID: p.FaceID, PersonID: p.PersonID, Dist: dist})
		ts.DistinctPersons[p.PersonID] = true
	}

	negatives, err := queryFacePersonPairs(db, `SELECT face_id, person_id FROM person_negatives`)
	if err != nil {
		return KNNTruthSet{}, err
	}
	for _, p := range negatives {
		vec, ok := vecByFace[p.FaceID]
		if !ok {
			ts.NegSkippedNoFace++
			continue
		}
		dist, ok := knnDistance(exemplarsByPerson[p.PersonID], p.FaceID, vec, p.PersonID, k)
		if !ok {
			ts.NegSkippedNoExemplar++
			continue
		}
		ts.Negatives = append(ts.Negatives, KNNTruthRow{FaceID: p.FaceID, PersonID: p.PersonID, Dist: dist})
		ts.DistinctPersons[p.PersonID] = true
	}

	return ts, nil
}

// KNNComboResult is one (T_auto, T_suggest) grid combo's report row.
type KNNComboResult struct {
	TAuto, TSuggest float64

	FalseAccept int // negatives with dist <= TAuto (wrongly auto-joined)
	TrueAccept  int // positives with dist <= TAuto (correctly auto-joined)

	GrayPositives int // TAuto < dist < TSuggest (correctly queued for review)
	GrayNegatives int // TAuto < dist < TSuggest (correctly kept out of auto, caught for review)

	Miss int // positives with dist >= TSuggest (never even reach a suggestion)
}

// knnFrange returns lo, lo+step, ..., hi inclusive, rounded to 2 decimals to
// avoid float64 accumulation drift (e.g. repeated +0.01 landing on
// 0.3399999999999999 instead of 0.34, which would otherwise break grid
// threshold comparisons and any downstream equality checks).
func knnFrange(lo, hi, step float64) []float64 {
	n := int(math.Round((hi-lo)/step)) + 1
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = math.Round((lo+float64(i)*step)*100) / 100
	}
	return out
}

// KNNGridScan sweeps the T_auto x T_suggest grid, classifying every
// positive/negative truth row's already-computed distance into the
// auto/gray/miss zones for each combo.
func KNNGridScan(positives, negatives []KNNTruthRow) []KNNComboResult {
	var results []KNNComboResult
	for _, ta := range knnFrange(knnTAutoLo, knnTAutoHi, knnTAutoStep) {
		falseAccept, trueAccept := 0, 0
		for _, r := range negatives {
			if r.Dist <= ta {
				falseAccept++
			}
		}
		for _, r := range positives {
			if r.Dist <= ta {
				trueAccept++
			}
		}

		tsLo := math.Round((ta+knnTSugMargin)*100) / 100
		if tsLo > knnTSugHi {
			continue // grid exhausted (not reachable with the current bounds/margin, kept defensive)
		}
		for _, ts := range knnFrange(tsLo, knnTSugHi, knnTSugStep) {
			grayPos, grayNeg, miss := 0, 0, 0
			for _, r := range positives {
				switch {
				case r.Dist <= ta:
					// counted in trueAccept above
				case r.Dist < ts:
					grayPos++
				default:
					miss++
				}
			}
			for _, r := range negatives {
				if r.Dist > ta && r.Dist < ts {
					grayNeg++
				}
			}
			results = append(results, KNNComboResult{
				TAuto: ta, TSuggest: ts,
				FalseAccept: falseAccept, TrueAccept: trueAccept,
				GrayPositives: grayPos, GrayNegatives: grayNeg,
				Miss: miss,
			})
		}
	}
	return results
}

// SortKNNResults orders combos per the selection criterion: zero
// false-accepts in the auto zone first, then maximize true-accepts, tie-break
// on larger gray-zone coverage of negatives (more of the "not this person"
// truth correctly routed to human review instead of being silently missed
// entirely), and finally a stable (T_auto, T_suggest) order for determinism
// among true ties.
func SortKNNResults(rs []KNNComboResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		zi, zj := rs[i].FalseAccept == 0, rs[j].FalseAccept == 0
		if zi != zj {
			return zi
		}
		if rs[i].TrueAccept != rs[j].TrueAccept {
			return rs[i].TrueAccept > rs[j].TrueAccept
		}
		if rs[i].GrayNegatives != rs[j].GrayNegatives {
			return rs[i].GrayNegatives > rs[j].GrayNegatives
		}
		if rs[i].TAuto != rs[j].TAuto {
			return rs[i].TAuto < rs[j].TAuto
		}
		return rs[i].TSuggest < rs[j].TSuggest
	})
}

// KNNInsufficient applies the requirement's three bars.
func KNNInsufficient(nPositives, nNegatives, nPersons int) bool {
	return nPositives < KNNMinPositives || nNegatives < KNNMinNegatives || nPersons < KNNMinPersons
}

// SelectKNNCombo returns the winning combo after rs has already been sorted
// by SortKNNResults: the first result, provided it has FalseAccept == 0 (the
// zero-false-accept hard constraint). An empty grid, or a grid where every
// combo leaks at least one negative into the auto zone, yields ok=false.
func SelectKNNCombo(rs []KNNComboResult) (tAuto, tSuggest float64, ok bool) {
	if len(rs) == 0 || rs[0].FalseAccept != 0 {
		return 0, 0, false
	}
	return rs[0].TAuto, rs[0].TSuggest, true
}
