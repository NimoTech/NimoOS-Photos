package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// TestRunPipelineNoOpWhenNothingChanged: steady state (all faces scanned &
// assigned) — a repeated RunPipeline must be a complete no-op: same person
// id (no daily UUID churn from rebuilding persons).
func TestRunPipelineNoOpWhenNothingChanged(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pidBefore string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pidBefore))

	// Mark the asset fully scanned so RunPipeline sees no detection targets.
	_, err := db.Exec(`UPDATE assets SET face_scanned=1, status='indexed'`)
	require.NoError(t, err)

	require.NoError(t, fs.RunPipeline(context.Background()))

	var pidAfter string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pidAfter))
	require.Equal(t, pidBefore, pidAfter,
		"daily pipeline with no new faces must not rebuild persons (UUID churn)")
}
