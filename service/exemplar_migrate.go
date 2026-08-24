package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// exemplarInitMarkerFile is the one-shot marker for the exemplar-assignment
// migration, written into a FaceService's markerDir (see SetMarkerDir) once
// the first exemplar-engine revalidation pass has completed. Mirrors
// quality_backfill.go's sharpnessBackfillMarkerFile pattern exactly.
const exemplarInitMarkerFile = ".exemplar_init_v1.done"

// The migration itself (brief §3.9 / Task 7 of the exemplar-assignment plan)
// has three steps, applied once, the first time an "apple"-engine clustering
// pass runs after this feature's deploy:
//
//  1. Every existing anchored person's members are conceptually "downgraded"
//     to confirmed=0 -- they came from the old centroid-snap dbscan
//     behavior, not from a real user confirming each face, and a person's
//     NAME anchors the cluster as a whole, not every individual member face.
//  2. Immediately run one exemplar-selection + KNN-assignment pass (i.e.
//     RunClustering, below).
//  3. That pass's own revalidation (rebuildPersonsWithProgress's step 1.5)
//     must not detach anyone: members beyond suggestDist are demoted to an
//     open 'review' suggestion instead of being silently dropped, so a human
//     gets to decide in the suggestions queue. Implemented as `lossless` in
//     rebuildPersonsWithProgress -- see that function and
//     exemplarMigrationLosslessPass/writeExemplarMigrationMarker below.
//
// Step ① is deliberately NOT implemented as a bulk UPDATE anywhere in this
// codebase. face_person.confirmed is a column this same feature branch added
// (`ALTER TABLE face_person ADD COLUMN confirmed INTEGER NOT NULL DEFAULT 0`,
// pkg/sqlite/db.go) -- every row that existed before this ALTER already
// became confirmed=0 by that default, and the ONLY code path anywhere that
// ever writes confirmed=1 is PersonService.decideSuggestion's accept branch
// (persons.go), which itself only has rows to accept once THIS migration's
// own first pass (step ②/③) has populated person_suggestions. In other
// words: by the time this migration runs, nothing could possibly have set
// confirmed=1 yet, so a bulk "SET confirmed=0" would touch zero rows by
// construction. Writing that dead UPDATE just to mirror the original plan's
// wording would be pure noise; this comment (and the exemplar-assignment
// data-flow section of OVERVIEW.md) documents the finding instead.

// exemplarMigrationLosslessPass reports whether rebuildPersonsWithProgress's
// step 1.5 revalidation must run in the migration's "lossless first pass"
// mode for THIS call. This is intentionally a fresh filesystem check every
// time, not cached service state: the marker file is the one and only source
// of truth for "has the migration's first pass already happened", and a
// cached in-memory bool would go stale across a process restart (RunClustering
// itself is service state that resets on every restart, which is exactly why
// it cannot be where this flag lives). Returns false when markerDir is empty
// (migration-awareness disabled -- see the FaceService.markerDir field doc
// comment) or when the marker file already exists.
func exemplarMigrationLosslessPass(markerDir string) bool {
	if markerDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(markerDir, exemplarInitMarkerFile))
	return errors.Is(err, os.ErrNotExist)
}

// writeExemplarMigrationMarker persists the exemplar-assignment migration's
// marker, so every subsequent rebuildPersonsWithProgress call reverts to
// normal (non-lossless) revalidation behavior. Called by
// rebuildPersonsWithProgress only after its transaction has committed
// successfully (see that function) -- never before, since writing the marker
// ahead of a successful commit could mark the migration "done" while this
// pass's own lossless demotions never actually persisted. No-op (silently)
// when markerDir is empty; a write failure is logged, not returned, matching
// BackfillSharpness's marker-write error handling (a failed write just means
// the next pass retries the lossless behavior, which is safe, not harmful).
func writeExemplarMigrationMarker(markerDir string) {
	if markerDir == "" {
		return
	}
	marker := filepath.Join(markerDir, exemplarInitMarkerFile)
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("migrated_at=%s\n", time.Now().Format(time.RFC3339))), 0o644); err != nil {
		zap.L().Warn("failed to write exemplar-assignment migration marker", zap.Error(err))
	}
}

// RunExemplarMigration is the one-shot startup driver for the exemplar-
// assignment migration (step ②/③ above): it makes sure the lossless first
// revalidation pass actually runs promptly after this deploy, rather than
// depending on some incidental future trigger (an upload, the safety-net
// scheduler, a manual recluster, ...) to eventually kick off clustering.
//
// Marker-guarded like BackfillSharpness: a no-op once .exemplar_init_v1.done
// exists in s.markerDir, and also a no-op when markerDir was never
// configured via SetMarkerDir at all (matches exemplarMigrationLosslessPass's
// convention -- no marker directory means migration-awareness is off, e.g.
// most unit tests). Unlike BackfillSharpness, this takes no markerDir
// parameter: s.markerDir must already be set (service.NewService wires it
// from cfg.DataPath alongside SetThumbDir), since rebuildPersonsWithProgress
// needs that same directory configured for EVERY clustering call during the
// migration window, not just this one explicit kick -- an incidental
// RunClustering triggered by, say, an upload arriving before this function
// gets around to running must be just as lossless as this call.
//
// Calling RunClustering here is what satisfies "immediately run one exemplar
// pass" -- the actual lossless-vs-normal decision for that pass's
// revalidation is made inside rebuildPersonsWithProgress itself (see
// exemplarMigrationLosslessPass), and the marker is written there too, only
// after that pass's transaction commits.
func (s *FaceService) RunExemplarMigration(ctx context.Context) error {
	if s.markerDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.markerDir, exemplarInitMarkerFile)); err == nil {
		return nil // already migrated
	}
	return s.RunClustering(ctx)
}
