package service_test

import (
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// Low-cohesion unnamed clusters (DBSCAN chaining "garbage bin") must not be
// exposed by ListPersons; named persons pass regardless of confidence.
func TestListPersonsConfidenceGate(t *testing.T) {
	db := makeTestFaceDB(t)
	// Two persons inserted directly: an unnamed garbage cluster (conf 0.3)
	// and a named low-confidence one (must survive the gate).
	_, err := db.Exec(`INSERT INTO persons(id, name, confidence) VALUES
		('garbage', '', 0.3),
		('alice',   'Alice', 0.2)`)
	require.NoError(t, err)

	old := config.Cfg
	config.Cfg = &config.Config{MinPersonConfidence: 0.5}
	defer func() { config.Cfg = old }()

	list, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "alice", list[0].ID)
}

// The same gate must keep garbage clusters out of co-appearance relations.
func TestPersonRelationsConfidenceGate(t *testing.T) {
	db := makeTestFaceDB(t)
	vecA := make([]float32, 512)
	vecA[0] = 1.0
	vecB := make([]float32, 512)
	vecB[1] = 1.0
	// Two faces on the same asset -> co-appearance between their persons.
	insertAssetFace(t, db, "shared", normalize(vecA))
	insertFaceOnAsset(t, db, "shared", normalize(vecB)) // see step 3 note if helper absent
	_, err := db.Exec(`INSERT INTO persons(id, name, confidence) VALUES
		('main', 'Alice', 0.9), ('garbage', '', 0.3)`)
	require.NoError(t, err)
	var f1, f2 string
	require.NoError(t, db.QueryRow(
		`SELECT id FROM face_detections ORDER BY rowid LIMIT 1`).Scan(&f1))
	require.NoError(t, db.QueryRow(
		`SELECT id FROM face_detections ORDER BY rowid DESC LIMIT 1`).Scan(&f2))
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, 'main'), (?, 'garbage')`, f1, f2)
	require.NoError(t, err)

	old := config.Cfg
	config.Cfg = &config.Config{MinPersonConfidence: 0.5}
	defer func() { config.Cfg = old }()

	rels, err := service.NewPersonService(db).PersonRelations("main")
	require.NoError(t, err)
	require.Empty(t, rels, "garbage cluster must not appear as a relation")
}
