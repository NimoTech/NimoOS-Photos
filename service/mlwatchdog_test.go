package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	running    bool
	runningErr error
	inspects   int
	restarts   int
	restartErr error
}

func (f *fakeRunner) IsRunning(ctx context.Context, name string) (bool, error) {
	f.inspects++
	return f.running, f.runningErr
}
func (f *fakeRunner) Restart(ctx context.Context, name string) error {
	f.restarts++
	return f.restartErr
}

// tickN runs Tick n times in a row, returning the cumulative number of restarts triggered.
func tickN(w *MLWatchdog, n int) int {
	restarted := 0
	for i := 0; i < n; i++ {
		if w.Tick(context.Background()) {
			restarted++
		}
	}
	return restarted
}

func TestWatchdogRestartsWedgedContainer(t *testing.T) {
	r := &fakeRunner{running: true}
	ready := false
	w := NewMLWatchdog(func() bool { return ready }, r)
	// Consecutive failures below the threshold (mlWatchdogFailLimit=12): no restart
	require.Equal(t, 0, tickN(w, 11))
	require.Equal(t, 0, r.restarts)
	// The 12th failure: triggers a restart
	require.True(t, w.Tick(context.Background()))
	require.Equal(t, 1, r.restarts)
}

func TestWatchdogResetsOnRecovery(t *testing.T) {
	r := &fakeRunner{running: true}
	ready := false
	w := NewMLWatchdog(func() bool { return ready }, r)
	tickN(w, 11)
	ready = true // recovered
	require.False(t, w.Tick(context.Background()))
	ready = false // fails again, the counter should accumulate from 0 again
	require.Equal(t, 0, tickN(w, 11))
	require.Equal(t, 0, r.restarts)
}

func TestWatchdogSkipsWhenContainerNotRunning(t *testing.T) {
	r := &fakeRunner{running: false} // not installed / manually stopped
	w := NewMLWatchdog(func() bool { return false }, r)
	require.Equal(t, 0, tickN(w, 30))
	require.Equal(t, 0, r.restarts)
}

func TestWatchdogCooldownBlocksSecondRestart(t *testing.T) {
	r := &fakeRunner{running: true}
	now := time.Unix(1000, 0)
	w := NewMLWatchdog(func() bool { return false }, r)
	w.now = func() time.Time { return now }
	tickN(w, 12)
	require.Equal(t, 1, r.restarts)
	// Continuing to fail during the cooldown: no further restart
	tickN(w, 24)
	require.Equal(t, 1, r.restarts)
	// After the cooldown: another restart is allowed
	now = now.Add(mlWatchdogCooldown + time.Second)
	tickN(w, 12)
	require.Equal(t, 2, r.restarts)
}

func TestWatchdogToleratesRestartError(t *testing.T) {
	r := &fakeRunner{running: true, restartErr: context.DeadlineExceeded}
	w := NewMLWatchdog(func() bool { return false }, r)
	require.Equal(t, 0, tickN(w, 12)) // Tick returns false when Restart errors, no panic
	require.Equal(t, 1, r.restarts)   // but it did attempt one
}

func TestWatchdogInspectCadenceWhenContainerNotRunning(t *testing.T) {
	r := &fakeRunner{running: false}
	w := NewMLWatchdog(func() bool { return false }, r)
	// 36 ticks (18 minutes): should inspect exactly once each at the
	// 12th/24th/36th tick, not fork a docker inspect on every tick once
	// fails is stuck at the threshold
	require.Equal(t, 0, tickN(w, 36))
	require.Equal(t, 3, r.inspects)
	require.Equal(t, 0, r.restarts)
}
