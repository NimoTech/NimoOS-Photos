package service

// DB-facing tests for generateMergeSuggestionsTx: the transaction plumbing
// around the pure generateMergeCandidates (unit-tested in isolation in
// merge_questions_internal_test.go) -- upsert/immutability, dead-id cleanup,
// and negative-pair suppression sourced from a real face_negative_pairs
// table rather than a hand-built map.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func mqOpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "mq.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// mqInsertPersonWithFace inserts a minimal person + single member face
// directly (bypassing the clustering pipeline), returning the new face id.
func mqInsertPersonWithFace(t *testing.T, db *sql.DB, personID, name string, vec []float32) string {
	t.Helper()
	faceID := uuid.NewString()
	assetID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?, 'indexed')`, assetID, "/x/"+assetID+".jpg")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name) VALUES(?, ?)`, personID, name)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, ?)`, faceID, personID)
	require.NoError(t, err)
	return faceID
}

// TestGenerateMergeSuggestionsTx_CreatesOpenRowForGrayBandPair: two anchored
// (named) persons... no -- exactly one named, so the pair isn't excluded --
// whose complete-linkage distance is in the gray band must get an open
// merge_suggestions row after generateMergeSuggestionsTx runs.
func TestGenerateMergeSuggestionsTx_CreatesOpenRowForGrayBandPair(t *testing.T) {
	db := mqOpenTestDB(t)
	dim := 8
	va, vb := vecAtCosineDistance(dim, mergeEps()+0.02)
	pidA := "pA"
	pidB := "pB"
	mqInsertPersonWithFace(t, db, pidA, "Alice", va)
	mqInsertPersonWithFace(t, db, pidB, "", vb)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx, nil, []string{pidA, pidB}))
	require.NoError(t, tx.Commit())

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions WHERE status='open'`).Scan(&cnt))
	require.Equal(t, 1, cnt)

	var personA, personB string
	var intoIsA int
	require.NoError(t, db.QueryRow(`SELECT person_a, person_b, into_is_a FROM merge_suggestions WHERE status='open'`).
		Scan(&personA, &personB, &intoIsA))
	fromID, intoID := resolveMergeSuggestionDirection(personA, personB, intoIsA != 0)
	require.Equal(t, pidB, fromID, "the unnamed side must be 'from'")
	require.Equal(t, pidA, intoID, "the named side must be 'into'")
}

// TestGenerateMergeSuggestionsTx_NamedNamedPairNotSuggested: two named
// anchored persons in the gray band must produce no row at all.
func TestGenerateMergeSuggestionsTx_NamedNamedPairNotSuggested(t *testing.T) {
	db := mqOpenTestDB(t)
	dim := 8
	va, vb := vecAtCosineDistance(dim, mergeEps()+0.02)
	pidA, pidB := "pA", "pB"
	mqInsertPersonWithFace(t, db, pidA, "Alice", va)
	mqInsertPersonWithFace(t, db, pidB, "Bob", vb)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx, nil, []string{pidA, pidB}))
	require.NoError(t, tx.Commit())

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions`).Scan(&cnt))
	require.Equal(t, 0, cnt)
}

// TestGenerateMergeSuggestionsTx_NegativeSuppressionFromRealTable: a
// face_negative_pairs row (as RejectMergeSuggestion would write) between the
// two clusters' member faces must suppress the pair even though its
// distance is in the gray band.
func TestGenerateMergeSuggestionsTx_NegativeSuppressionFromRealTable(t *testing.T) {
	db := mqOpenTestDB(t)
	dim := 8
	va, vb := vecAtCosineDistance(dim, mergeEps()+0.02)
	pidA, pidB := "pA", "pB"
	faceA := mqInsertPersonWithFace(t, db, pidA, "", va)
	faceB := mqInsertPersonWithFace(t, db, pidB, "", vb)

	a, b := orderPair(faceA, faceB)
	_, err := db.Exec(`INSERT INTO face_negative_pairs(face_a, face_b, created_at) VALUES(?,?,CURRENT_TIMESTAMP)`, a, b)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx, nil, []string{pidA, pidB}))
	require.NoError(t, tx.Commit())

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions`).Scan(&cnt))
	require.Equal(t, 0, cnt, "a negative-linked pair must not be (re)suggested")
}

// TestGenerateMergeSuggestionsTx_DeadIDCleanup: an open merge_suggestions row
// referencing a person id that no longer exists (simulating an auto person
// rebuilt with a new id on a later pass) must be deleted.
func TestGenerateMergeSuggestionsTx_DeadIDCleanup(t *testing.T) {
	db := mqOpenTestDB(t)
	_, err := db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, status, created_at)
		VALUES('stale','dead-from','dead-into',0,0.58,'open',CURRENT_TIMESTAMP)`)
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx, nil, nil))
	require.NoError(t, tx.Commit())

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions WHERE id='stale'`).Scan(&cnt))
	require.Equal(t, 0, cnt, "an open row referencing a since-deleted person id must be cleaned up")
}

// TestGenerateMergeSuggestionsTx_DecidedRowImmutable: once a candidate pair's
// row has been decided (accepted/rejected), a later pass re-generating the
// same pair must not reopen it or touch its dist.
func TestGenerateMergeSuggestionsTx_DecidedRowImmutable(t *testing.T) {
	db := mqOpenTestDB(t)
	dim := 8
	va, vb := vecAtCosineDistance(dim, mergeEps()+0.02)
	pidA, pidB := "pA", "pB"
	mqInsertPersonWithFace(t, db, pidA, "", va)
	mqInsertPersonWithFace(t, db, pidB, "", vb)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx, nil, []string{pidA, pidB}))
	require.NoError(t, tx.Commit())

	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM merge_suggestions`).Scan(&id))
	_, err = db.Exec(`UPDATE merge_suggestions SET status='rejected', decided_at=CURRENT_TIMESTAMP, dist=0.999 WHERE id=?`, id)
	require.NoError(t, err)

	// Re-run generation for the exact same pair/distance.
	tx2, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, generateMergeSuggestionsTx(context.Background(), tx2, nil, []string{pidA, pidB}))
	require.NoError(t, tx2.Commit())

	var status string
	var dist float64
	require.NoError(t, db.QueryRow(`SELECT status, dist FROM merge_suggestions WHERE id=?`, id).Scan(&status, &dist))
	require.Equal(t, "rejected", status, "a decided row must stay decided")
	require.Equal(t, 0.999, dist, "a decided row's dist must not be overwritten by a later pass")

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions`).Scan(&cnt))
	require.Equal(t, 1, cnt, "no duplicate row should be inserted for the same pair")
}
