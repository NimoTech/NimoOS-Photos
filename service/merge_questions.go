package service

// Cluster-merge questions: the apple engine's pass-2 HAC (see mergeEps in
// faces.go) intentionally stops merging once the closest remaining pair of
// clusters exceeds ClusterMergeEps, to stay chaining-resistant. Pairs just
// ABOVE that stop line are natural "almost merged" candidates -- rather than
// silently leaving them fragmented, this surfaces them as a review queue:
// accept merges the two clusters right now (no naming needed, since neither
// side may even have a name yet); reject pins a durable face-level
// cannot-link so the same pairing doesn't keep coming back every pass.
//
// This is a distinct subsystem from both the legacy on-the-fly
// GET /persons/merge-suggestions (service/persons.go's MergeSuggestions,
// computed fresh from named-cluster centroids on every call) and the
// join/review exemplar-assignment queue (person_suggestions) -- see
// OVERVIEW.md's "Cluster-merge questions" section for the full picture.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
)

// mergeSuggestBand returns the configured width (in cosine distance) of the
// gray band above ClusterMergeEps that a pass-final person pair's
// complete-linkage distance must fall into to be surfaced as a cluster-merge
// question. Falls back to 0.06 when config isn't initialized or the value is
// non-positive.
func mergeSuggestBand() float64 {
	if config.Cfg != nil && config.Cfg.MergeSuggestBand > 0 {
		return config.Cfg.MergeSuggestBand
	}
	return 0.06
}

// mergeSuggestCap returns the configured max number of gray-band candidates
// kept (closest dist first) per clustering pass. Falls back to 30 when
// config isn't initialized or the value is non-positive.
func mergeSuggestCap() int {
	if config.Cfg != nil && config.Cfg.MergeSuggestCap > 0 {
		return config.Cfg.MergeSuggestCap
	}
	return 30
}

// mcPerson is one person's data needed for gray-band candidate generation:
// enough to compute a complete-linkage distance against another person and
// to decide direction/exclusion (name) and cannot-link suppression
// (faceIDs). Built once per pass from two sources -- see
// generateMergeSuggestionsTx: freshly created auto persons carry their
// member vectors already in memory from step 4's clustering loop; anchored
// persons are loaded fresh via loadAnchoredMemberSets.
type mcPerson struct {
	id       string
	name     string // "" for every auto person; anchored persons may or may not be named
	faceIDs  []string
	vecs     [][]float32
	centroid []float32
}

// mcCandidate is one gray-band person pair, direction already resolved.
type mcCandidate struct {
	fromID, intoID string
	dist           float64
}

// completeLinkageDist returns the complete-linkage (max pairwise cosine)
// distance between two member-vector sets, mirroring HACComplete's own
// inter-cluster distance definition (service/cluster_engine.go) so a
// gray-band candidate here is asking exactly the question pass-2's HAC
// itself almost answered "yes" to.
func completeLinkageDist(a, b [][]float32) float64 {
	var maxd float64
	for _, va := range a {
		for _, vb := range b {
			if d := cosDist(va, vb); d > maxd {
				maxd = d
			}
		}
	}
	return maxd
}

// facePairNegated reports whether any (face from faceA, face from faceB)
// pair is present in the negative-pair set (face_negative_pairs, loaded once
// per pass by loadFaceNegativePairSet). Cost is O(|faceA|*|faceB|) map
// lookups, but only paid for pairs that already survived the centroid
// prefilter + distance-band check, so the cross product here is always over
// a small candidate set, never the full person-pair space.
func facePairNegated(faceA, faceB []string, neg map[string]bool) bool {
	if len(neg) == 0 {
		return false
	}
	for _, fa := range faceA {
		for _, fb := range faceB {
			if neg[pairKey(fa, fb)] {
				return true
			}
		}
	}
	return false
}

// mergeDirection resolves which side is "into" (the merge target) and which
// is "from" (absorbed): the named side wins when exactly one side is named
// (named<->named pairs are excluded before this is ever called, by
// generateMergeCandidates); otherwise the larger cluster (by member count)
// wins; a tie (equal size, both unnamed) breaks on id for determinism.
func mergeDirection(a, b mcPerson) (fromID, intoID string) {
	aNamed, bNamed := a.name != "", b.name != ""
	switch {
	case aNamed && !bNamed:
		return b.id, a.id
	case bNamed && !aNamed:
		return a.id, b.id
	case len(a.vecs) > len(b.vecs):
		return b.id, a.id
	case len(b.vecs) > len(a.vecs):
		return a.id, b.id
	default:
		if a.id > b.id {
			return b.id, a.id
		}
		return a.id, b.id
	}
}

// generateMergeCandidates is the pure (no DB access) core of gray-band
// candidate generation, deliberately factored out so it can be unit- and
// performance-tested in isolation from the transaction plumbing (mirrors
// HACComplete's own pure-function shape in cluster_engine.go).
//
// Pruning: complete-linkage distance is not a mathematically strict function
// of centroid distance in full generality, but for L2-normalized embeddings
// there IS a derivable bound covering the case that matters here. Write
// meanSim/minSim for the mean/min pairwise dot-product similarity over all
// (vecA_i, vecB_j) cross pairs; complete-linkage distance is 1-minSim by
// definition. mean >= min unconditionally, so 1-meanSim <= 1-minSim always.
// Separately, cosDist(centroidA, centroidB) = 1 - meanSim/(|centroidA|
// *|centroidB|), and since each centroid's norm is <=1 (it's a mean of unit
// vectors), meanSim/(|centroidA|*|centroidB|) >= meanSim WHENEVER meanSim is
// non-negative -- giving cosDist(centroidA, centroidB) <= 1-meanSim.
// Chaining both: whenever the average cross-pair similarity is
// non-negative, cosDist(centroidA, centroidB) <= 1-meanSim <= complete-
// linkage distance, i.e. the centroid distance is a genuine (not just
// heuristic) lower bound. Every pair actually reachable in-band has
// per-pair similarity >= 1-ceiling (empirically >= ~0.35 at the default
// thresholds), comfortably non-negative, so pruneSlack's 0.2 is roughly 10x
// more conservative than this bound requires. The one precondition this
// leans on -- L2-normalized face embeddings -- is an assumption of the
// ArcFace-family embedding model feeding this pipeline, NOT something
// asserted anywhere in this code: shrinking pruneSlack below its current
// generous value should not happen without first adding an explicit
// normalization check, since a future embedding-model swap could silently
// violate the precondition this whole argument depends on. pruneSlack (0.2)
// trades a (per the above, now well-bounded) small residual risk for a
// large cut in the O(P^2) candidate scan at production scale (P ~1200 final
// persons per pass -- see the 2026-08-20 production run noted in
// OVERVIEW.md). See TestGenerateMergeCandidates_PerformanceAt1200ClusterScale
// for the measured wall-time impact this pruning buys back.
func generateMergeCandidates(persons []mcPerson, eps, band float64, maxCandidates int, neg map[string]bool) []mcCandidate {
	ceiling := eps + band
	const pruneSlack = 0.2
	pruneCeiling := ceiling + pruneSlack

	var candidates []mcCandidate
	for i := 0; i < len(persons); i++ {
		for j := i + 1; j < len(persons); j++ {
			a, b := persons[i], persons[j]
			if a.name != "" && b.name != "" {
				continue // named<->named pairs are never auto-suggested (standing rule)
			}
			if len(a.vecs) == 0 || len(b.vecs) == 0 {
				continue // defensive: no members, no meaningful complete-linkage distance
			}
			if cosDist(a.centroid, b.centroid) > pruneCeiling {
				continue
			}
			d := completeLinkageDist(a.vecs, b.vecs)
			if d <= eps || d > ceiling {
				continue
			}
			if facePairNegated(a.faceIDs, b.faceIDs, neg) {
				continue
			}
			fromID, intoID := mergeDirection(a, b)
			candidates = append(candidates, mcCandidate{fromID: fromID, intoID: intoID, dist: d})
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].dist < candidates[j].dist })
	if maxCandidates > 0 && len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	return candidates
}

// loadAnchoredMemberSets loads the current (post step-1.5-revalidation)
// active member faces/vectors for each anchored person id, in one grouped
// query -- deliberately a fresh query rather than threading step 1's
// per-person vecByFace state through revalidation/step-2/step-3, since that
// state is invalidated by step 1.5's detachments and re-deriving it inline
// would couple this function to revalidation's internals for no benefit at
// the person counts involved (~1200 in production). Hidden persons are
// excluded entirely: a hidden person is effectively soft-deleted, and a
// cluster-merge question involving it would surface a suggestion for a
// person the user can't act on without first restoring it.
func loadAnchoredMemberSets(ctx context.Context, tx *sql.Tx, anchorIDs []string) ([]mcPerson, error) {
	if len(anchorIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(anchorIDs))
	args := make([]any, len(anchorIDs))
	for i, id := range anchorIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, p.name, fd.id, fd.embedding
		FROM persons p
		JOIN face_person fp ON fp.person_id = p.id
		JOIN face_detections fd ON fd.id = fp.face_id AND fd.excluded = 0
		WHERE p.id IN (`+strings.Join(placeholders, ",")+`) AND p.hidden = 0`, args...)
	if err != nil {
		return nil, fmt.Errorf("loadAnchoredMemberSets: %w", err)
	}
	defer rows.Close()

	byID := map[string]*mcPerson{}
	var order []string
	for rows.Next() {
		var pid, name, faceID string
		var blob []byte
		if err := rows.Scan(&pid, &name, &faceID, &blob); err != nil {
			return nil, err
		}
		pm, ok := byID[pid]
		if !ok {
			pm = &mcPerson{id: pid, name: name}
			byID[pid] = pm
			order = append(order, pid)
		}
		pm.faceIDs = append(pm.faceIDs, faceID)
		pm.vecs = append(pm.vecs, sqlite.DeserializeFloat32(blob))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]mcPerson, 0, len(order))
	for _, pid := range order {
		pm := byID[pid]
		pm.centroid = ComputeCentroid(pm.vecs)
		out = append(out, *pm)
	}
	return out, nil
}

// loadFaceNegativePairSet loads every durable cannot-link pair
// (face_negative_pairs, written by RejectMergeSuggestion) into a lookup set
// keyed by pairKey (direction-independent). Cost is O(total negative pairs)
// -- expected to stay small across the DB's lifetime, since a row is only
// ever added by an explicit human rejection, not by any bulk process.
func loadFaceNegativePairSet(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT face_a, face_b FROM face_negative_pairs`)
	if err != nil {
		return nil, fmt.Errorf("loadFaceNegativePairSet: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[pairKey(a, b)] = true
	}
	return out, rows.Err()
}

// mergeSuggestionDirection canonicalizes a resolved (fromID, intoID) pair
// into the merge_suggestions row shape: personA/personB in orderPair order,
// plus intoIsA recording which of the two is the merge target. The inverse
// of this -- reading a row back into (fromID, intoID) -- is
// resolveMergeSuggestionDirection below. Keeping the pair canonical (rather
// than storing from/into directly) is what makes UNIQUE(person_a, person_b)
// collide on the SAME physical pair regardless of which side generation
// currently resolves as "into" -- see the merge_suggestions CREATE TABLE's
// doc comment in pkg/sqlite/db.go for the flip scenario this closes.
func mergeSuggestionDirection(fromID, intoID string) (personA, personB string, intoIsA bool) {
	personA, personB = orderPair(fromID, intoID)
	intoIsA = intoID == personA
	return personA, personB, intoIsA
}

// resolveMergeSuggestionDirection is the inverse of mergeSuggestionDirection:
// given a stored canonical row, returns which id is "from" (absorbed) and
// which is "into" (the merge target).
func resolveMergeSuggestionDirection(personA, personB string, intoIsA bool) (fromID, intoID string) {
	if intoIsA {
		return personB, personA
	}
	return personA, personB
}

// generateMergeSuggestionsTx generates and persists this pass's cluster-merge
// question candidates. Called from rebuildPersonsWithProgress (apple engine
// only) after step 5 (recomputePersonStatsTx), so persons.centroid/
// confidence already reflect this pass's exact final membership -- still
// inside the same transaction as the rest of the pass, so a rollback undoes
// suggestion writes along with everything else.
//
// autoPersons carries the free clusters this pass just created (step 4),
// already fully in memory from that step. anchorIDs is step 1's
// anchored-person id list; loadAnchoredMemberSets re-queries their current
// members fresh (see that function's doc comment for why).
//
// Cleanup runs first: DELETE any open row whose person_a/person_b no longer
// exists in persons. Auto person ids are rebuilt every pass (step 2 deletes
// every non-anchored person before step 4 recreates them from scratch), so
// without this, open rows referencing a dead id from a prior pass would
// accumulate forever.
func generateMergeSuggestionsTx(ctx context.Context, tx *sql.Tx, autoPersons []mcPerson, anchorIDs []string) error {
	anchored, err := loadAnchoredMemberSets(ctx, tx, anchorIDs)
	if err != nil {
		return err
	}
	all := make([]mcPerson, 0, len(autoPersons)+len(anchored))
	all = append(all, autoPersons...)
	all = append(all, anchored...)

	neg, err := loadFaceNegativePairSet(ctx, tx)
	if err != nil {
		return err
	}

	candidates := generateMergeCandidates(all, mergeEps(), mergeSuggestBand(), mergeSuggestCap(), neg)

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM merge_suggestions
		WHERE status='open' AND (
			person_a NOT IN (SELECT id FROM persons) OR
			person_b NOT IN (SELECT id FROM persons)
		)`); err != nil {
		return fmt.Errorf("generateMergeSuggestionsTx cleanup: %w", err)
	}

	if len(candidates) == 0 {
		return nil
	}

	// into_is_a is refreshed alongside dist on conflict, not just inserted
	// once: the pair's canonical key (person_a, person_b) doesn't encode
	// direction, so if a later pass resolves the opposite side as "into"
	// (e.g. the larger-cluster-wins-into rule flipping as member counts
	// change), this upsert still lands on the SAME row and keeps its
	// direction current, rather than the old directional-UNIQUE schema's
	// failure mode of silently accumulating a second row for the reverse
	// direction.
	upsertStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, status, created_at)
		VALUES(?, ?, ?, ?, ?, 'open', ?)
		ON CONFLICT(person_a, person_b) DO UPDATE SET dist=excluded.dist, into_is_a=excluded.into_is_a
			WHERE merge_suggestions.status='open'`)
	if err != nil {
		return err
	}
	defer upsertStmt.Close()

	now := time.Now()
	for _, c := range candidates {
		personA, personB, intoIsA := mergeSuggestionDirection(c.fromID, c.intoID)
		if _, err := upsertStmt.ExecContext(ctx, uuid.NewString(), personA, personB, intoIsA, c.dist, now); err != nil {
			return fmt.Errorf("generateMergeSuggestionsTx upsert: %w", err)
		}
	}
	return nil
}

// ── Cluster-merge questions read/decide API (PersonService) ───────────────

// MergeQuestion is one open cluster-merge question, the shape GET
// /persons/merge-suggestions/v2 returns (dist ascending). FromFaceIDs/
// IntoFaceIDs carry up to 4 quality-ordered preview faces per side (see
// mergeQuestionPreviewFacesQuery); FromFaces/IntoFaces are the same faces, in
// the same order, additionally carrying each face's asset id so the
// merge-card UI can open the full photo behind a preview face.
type MergeQuestion struct {
	ID          string        `json:"id"`
	Dist        float64       `json:"dist"`
	From        Person        `json:"from"`
	Into        Person        `json:"into"`
	FromFaceIDs []string      `json:"fromFaceIds"`
	IntoFaceIDs []string      `json:"intoFaceIds"`
	FromFaces   []PreviewFace `json:"fromFaces"`
	IntoFaces   []PreviewFace `json:"intoFaces"`
}

// PreviewFace identifies one preview face and the asset it belongs to, so a
// merge-question card can link a preview face thumbnail to the full photo
// behind it.
type PreviewFace struct {
	FaceID  string `json:"faceId"`
	AssetID string `json:"assetId"`
}

// mergeQuestionPreviewFacesQuery returns up to 4 of a person's active member
// faces (plus each face's asset id) for the merge-question card's preview
// strip, quality-ordered. Exemplar faces (face_person.exemplar=1) sort first
// when present; auto persons (rebuilt fresh every clustering pass) never
// carry exemplar flags, so for them this naturally falls back to the plain
// "fd.score DESC NULLS LAST, face id ASC" ordering exemplarFaceIDsQuery uses
// -- one query covers both cases instead of a separate fallback query.
const mergeQuestionPreviewFacesQuery = `
	SELECT fp.face_id, fd.asset_id
	FROM face_person fp
	JOIN face_detections fd ON fd.id = fp.face_id
	WHERE fp.person_id = ? AND fd.excluded = 0
	ORDER BY fp.exemplar DESC, fd.score DESC NULLS LAST, fp.face_id ASC
	LIMIT 4`

// previewFaces runs mergeQuestionPreviewFacesQuery for one person, returning
// [] (never nil) when the person currently has no active member faces.
func (s *PersonService) previewFaces(personID string) ([]PreviewFace, error) {
	rows, err := s.db.Query(mergeQuestionPreviewFacesQuery, personID)
	if err != nil {
		return nil, fmt.Errorf("previewFaces: %w", err)
	}
	defer rows.Close()
	faces := make([]PreviewFace, 0, 4)
	for rows.Next() {
		var f PreviewFace
		if err := rows.Scan(&f.FaceID, &f.AssetID); err != nil {
			return nil, err
		}
		faces = append(faces, f)
	}
	return faces, rows.Err()
}

// previewFaceIDs extracts just the face ids from previewFaces' result, in
// the same order, for the legacy FromFaceIDs/IntoFaceIDs fields.
func previewFaceIDs(faces []PreviewFace) []string {
	ids := make([]string, 0, len(faces))
	for _, f := range faces {
		ids = append(ids, f.FaceID)
	}
	return ids
}

// ListMergeQuestions returns every open cluster-merge question, dist
// ascending. Hidden persons are excluded on either side: GetPerson already
// scopes to hidden=0 and returns ErrNotFound otherwise, which this treats as
// "skip the row" rather than failing the whole list -- the same defensive
// posture as a since-deleted person id (generateMergeSuggestionsTx's own
// per-pass cleanup keeps this rare in practice, but a person can still be
// hidden after generation and before this call runs).
func (s *PersonService) ListMergeQuestions() ([]MergeQuestion, error) {
	rows, err := s.db.Query(`
		SELECT id, person_a, person_b, into_is_a, dist
		FROM merge_suggestions
		WHERE status='open'
		ORDER BY dist ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListMergeQuestions: %w", err)
	}
	type rawRow struct {
		id, fromID, intoID string
		dist               float64
	}
	var raw []rawRow
	for rows.Next() {
		var id, personA, personB string
		var intoIsA int
		var dist float64
		if err := rows.Scan(&id, &personA, &personB, &intoIsA, &dist); err != nil {
			rows.Close()
			return nil, err
		}
		fromID, intoID := resolveMergeSuggestionDirection(personA, personB, intoIsA != 0)
		raw = append(raw, rawRow{id: id, fromID: fromID, intoID: intoID, dist: dist})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]MergeQuestion, 0, len(raw))
	for _, r := range raw {
		from, ferr := s.GetPerson(r.fromID)
		if errors.Is(ferr, ErrNotFound) {
			continue
		}
		if ferr != nil {
			return nil, ferr
		}
		into, ierr := s.GetPerson(r.intoID)
		if errors.Is(ierr, ErrNotFound) {
			continue
		}
		if ierr != nil {
			return nil, ierr
		}
		fromFaces, err := s.previewFaces(r.fromID)
		if err != nil {
			return nil, err
		}
		intoFaces, err := s.previewFaces(r.intoID)
		if err != nil {
			return nil, err
		}
		out = append(out, MergeQuestion{
			ID: r.id, Dist: r.dist, From: *from, Into: *into,
			FromFaceIDs: previewFaceIDs(fromFaces), IntoFaceIDs: previewFaceIDs(intoFaces),
			FromFaces: fromFaces, IntoFaces: intoFaces,
		})
	}
	return out, nil
}

// representativeFaceTx returns the top-quality active member face of a
// person (exemplar-first, then score-ordered -- same ordering as
// mergeQuestionPreviewFacesQuery, just LIMIT 1), or "" if the person
// currently has none.
func representativeFaceTx(tx *sql.Tx, personID string) (string, error) {
	var faceID string
	err := tx.QueryRow(`
		SELECT fp.face_id
		FROM face_person fp
		JOIN face_detections fd ON fd.id = fp.face_id
		WHERE fp.person_id = ? AND fd.excluded = 0
		ORDER BY fp.exemplar DESC, fd.score DESC NULLS LAST, fp.face_id ASC
		LIMIT 1`, personID).Scan(&faceID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return faceID, nil
}

// loadOpenMergeSuggestionTx loads one merge_suggestions row's decision
// fields, shared by AcceptMergeSuggestion/RejectMergeSuggestion. Resolves
// the stored canonical (person_a, person_b, into_is_a) back into
// (fromID, intoID) so callers don't need to know about the canonical storage
// shape at all.
func loadOpenMergeSuggestionTx(tx *sql.Tx, id string) (fromID, intoID, status string, decidedAt sql.NullTime, err error) {
	var personA, personB string
	var intoIsA int
	err = tx.QueryRow(`
		SELECT person_a, person_b, into_is_a, status, decided_at
		FROM merge_suggestions WHERE id=?`, id).Scan(&personA, &personB, &intoIsA, &status, &decidedAt)
	if err == sql.ErrNoRows {
		err = ErrNotFound
		return
	}
	if err != nil {
		return
	}
	fromID, intoID = resolveMergeSuggestionDirection(personA, personB, intoIsA != 0)
	return
}

// AcceptMergeSuggestion accepts an open cluster-merge question: merges the
// resolved "from" person into the resolved "into" person (reusing
// mergePersonsTx -- the same tx-scoped machinery MergePersons uses, so
// stats are recomputed identically) and marks the row 'accepted', all in one
// transaction. Idempotent: a repeat
// call on an already-decided row is a no-op that returns its current
// (already-decided) state, mirroring decideSuggestion's contract.
func (s *PersonService) AcceptMergeSuggestion(id string) (result *SuggestionDecision, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	// committed tracks whether this call reached a successful tx.Commit, so
	// the deferred cleanup below only rolls back an in-flight transaction --
	// it must NOT fire after the dead-id branch's own commit further down,
	// which deliberately returns a non-nil error (ErrNotFound) alongside a
	// transaction that already committed successfully.
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck
		}
	}()

	fromID, intoID, status, decidedAt, lerr := loadOpenMergeSuggestionTx(tx, id)
	if lerr != nil {
		return nil, lerr
	}
	if status != "open" {
		d := &SuggestionDecision{ID: id, Status: status}
		if decidedAt.Valid {
			t := decidedAt.Time
			d.DecidedAt = &t
		}
		return d, nil
	}

	if mergeErr := mergePersonsTx(tx, fromID, intoID); mergeErr != nil {
		if errors.Is(mergeErr, ErrNotFound) {
			// fromID/intoID reference a person that no longer exists -- e.g.
			// a dead auto-person id from an earlier pass that this pass's own
			// generation cleanup hasn't caught yet. There is nothing to
			// merge; rather than leaving this now-useless open row for the
			// next pass's cleanup to eventually find, delete it eagerly in
			// this same transaction and still report 404 so the caller
			// knows the accept did not happen. Leaving it 'open' (the
			// no-op-rollback default) or marking it 'rejected' would both be
			// wrong: 'open' invites a repeat accept attempt against the same
			// dead id, and 'rejected' asserts a human decision that was
			// never made.
			if _, derr := tx.Exec(`DELETE FROM merge_suggestions WHERE id=?`, id); derr != nil {
				return nil, derr
			}
			if cerr := tx.Commit(); cerr != nil {
				return nil, cerr
			}
			committed = true
			return nil, ErrNotFound
		}
		return nil, mergeErr
	}
	now := time.Now()
	if _, uerr := tx.Exec(`UPDATE merge_suggestions SET status='accepted', decided_at=? WHERE id=?`, now, id); uerr != nil {
		return nil, uerr
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, cerr
	}
	committed = true
	return &SuggestionDecision{ID: id, Status: "accepted", DecidedAt: &now}, nil
}

// RejectMergeSuggestion rejects an open cluster-merge question: marks it
// 'rejected' and pins a durable cannot-link between the two clusters'
// top-quality representative faces (face_negative_pairs, normalized
// face_a<face_b via orderPair) so the pairing is suppressed on future
// passes even after the (likely auto, id-unstable) persons involved get
// rebuilt with new ids. If either side currently has no active member face
// (a rare race with a concurrent detach/delete), the negative pair is
// skipped -- there's no representative face left to pin -- but the question
// is still resolved. Idempotent like AcceptMergeSuggestion.
func (s *PersonService) RejectMergeSuggestion(id string) (result *SuggestionDecision, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	fromID, intoID, status, decidedAt, lerr := loadOpenMergeSuggestionTx(tx, id)
	if lerr != nil {
		err = lerr
		return nil, err
	}
	if status != "open" {
		tx.Rollback() //nolint:errcheck
		d := &SuggestionDecision{ID: id, Status: status}
		if decidedAt.Valid {
			t := decidedAt.Time
			d.DecidedAt = &t
		}
		return d, nil
	}

	now := time.Now()
	fromFace, ferr := representativeFaceTx(tx, fromID)
	if ferr != nil {
		err = ferr
		return nil, err
	}
	intoFace, ierr := representativeFaceTx(tx, intoID)
	if ierr != nil {
		err = ierr
		return nil, err
	}
	if fromFace != "" && intoFace != "" {
		a, b := orderPair(fromFace, intoFace)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO face_negative_pairs(face_a, face_b, created_at) VALUES(?,?,?)`, a, b, now); err != nil {
			return nil, err
		}
	}

	if _, err = tx.Exec(`UPDATE merge_suggestions SET status='rejected', decided_at=? WHERE id=?`, now, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &SuggestionDecision{ID: id, Status: "rejected", DecidedAt: &now}, nil
}
