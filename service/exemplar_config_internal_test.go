package service

import (
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// The exemplar/KNN-assignment accessors must fall back to their documented
// defaults when config.Cfg is nil (tests construct services without config),
// following the clusterEpsilon()/minPersonConfidence() nil-config-fallback
// pattern.
func TestExemplarAssignAccessorsDefaultWhenConfigNil(t *testing.T) {
	old := config.Cfg
	config.Cfg = nil
	defer func() { config.Cfg = old }()

	require.Equal(t, 24, exemplarCap())
	score, front, sharp := exemplarQualityGate()
	require.Equal(t, 0.75, score)
	require.Equal(t, 0.5, front)
	require.Equal(t, 0.3, sharp)
	require.Equal(t, 0.45, assignAutoDist())
	require.Equal(t, 0.60, assignSuggestDist())
	require.Equal(t, 5, assignK())
	require.Equal(t, 3, assignMinVotes())
}

// The accessors must also fall back to their documented defaults when
// config.Cfg is non-nil but the fields are left at their Go zero value
// (a config file predating these keys, same fallback shape as the apple
// engine's ClusterTightEps/ClusterMergeEps accessors).
func TestExemplarAssignAccessorsFallBackOnZeroValue(t *testing.T) {
	old := config.Cfg
	config.Cfg = &config.Config{}
	defer func() { config.Cfg = old }()

	require.Equal(t, 24, exemplarCap())
	score, front, sharp := exemplarQualityGate()
	require.Equal(t, 0.75, score)
	require.Equal(t, 0.5, front)
	require.Equal(t, 0.3, sharp)
	require.Equal(t, 0.45, assignAutoDist())
	require.Equal(t, 0.60, assignSuggestDist())
	require.Equal(t, 5, assignK())
	require.Equal(t, 3, assignMinVotes())
}

// Configured (non-zero) values must take effect.
func TestExemplarAssignAccessorsUseConfiguredValues(t *testing.T) {
	old := config.Cfg
	config.Cfg = &config.Config{
		ExemplarMaxPerPerson:  40,
		ExemplarMinScore:      0.8,
		ExemplarMinFrontality: 0.6,
		ExemplarMinSharpness:  0.4,
		AssignAutoDist:        0.4,
		AssignSuggestDist:     0.55,
		AssignKNNK:            7,
		AssignMinVotes:        4,
		// AssignAutoDist/AssignSuggestDist are calibratable thresholds:
		// resolveThreshold's four-layer stack only honors a conf value when
		// it's marked explicit, so this must be set for the config values
		// above to take effect.
		Explicit: map[string]bool{
			"AssignAutoDist":    true,
			"AssignSuggestDist": true,
		},
	}
	defer func() { config.Cfg = old }()

	require.Equal(t, 40, exemplarCap())
	score, front, sharp := exemplarQualityGate()
	require.Equal(t, 0.8, score)
	require.Equal(t, 0.6, front)
	require.Equal(t, 0.4, sharp)
	require.Equal(t, 0.4, assignAutoDist())
	require.Equal(t, 0.55, assignSuggestDist())
	require.Equal(t, 7, assignK())
	require.Equal(t, 4, assignMinVotes())
}
