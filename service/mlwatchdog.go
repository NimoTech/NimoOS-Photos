package service

import (
	"context"
	"time"

	"go.uber.org/zap"
)

const (
	mlWatchdogInterval = 30 * time.Second
	// 12 ticks (~6 minutes). The mlserver backend is a single-process
	// uvicorn app whose model loads no longer block /ping (unlike the old
	// gunicorn-fronted immich-ml, where a cold load could stall the shared
	// heartbeat for >120s and a 300s self-healing respawn was the original
	// reason this threshold was set this high). That self-heal window is
	// gone with this backend, but the value is kept unchanged on purpose:
	// it is a conservative second line of defense against container-level
	// wedges -- a runtime crash, a docker-level stall, anything that leaves
	// the container "running" per docker but unresponsive -- not a bound
	// tuned to any particular startup cost. Six minutes of consecutive
	// failures is cheap insurance against false-positive restarts and still
	// catches a genuinely wedged container promptly.
	mlWatchdogFailLimit = 12
	mlWatchdogCooldown  = 10 * time.Minute
	mlContainerName     = "nimoos-photos-ml-server-1"
)

// ContainerRunner abstracts the docker CLI so tests can fake it.
type ContainerRunner interface {
	IsRunning(ctx context.Context, name string) (bool, error)
	Restart(ctx context.Context, name string) error
}

// MLWatchdog restarts the ML container when it is alive-but-wedged:
// /ping keeps timing out while docker still reports the container running.
// compose's restart:unless-stopped only saves you from "the process exited",
// it can't save you from this kind of hang.
type MLWatchdog struct {
	ready       func() bool
	runner      ContainerRunner
	container   string
	fails       int
	lastRestart time.Time
	now         func() time.Time
}

func NewMLWatchdog(ready func() bool, runner ContainerRunner) *MLWatchdog {
	return &MLWatchdog{
		ready:     ready,
		runner:    runner,
		container: mlContainerName,
		now:       time.Now,
	}
}

// Tick runs one probe cycle; returns true if a restart was issued.
func (w *MLWatchdog) Tick(ctx context.Context) bool {
	if w.ready() {
		w.fails = 0
		return false
	}
	w.fails++
	if w.fails < mlWatchdogFailLimit {
		return false
	}
	if w.now().Sub(w.lastRestart) < mlWatchdogCooldown {
		return false
	}
	running, err := w.runner.IsRunning(ctx, w.container)
	if err != nil {
		zap.L().Warn("ml watchdog: docker inspect failed, cannot confirm container state",
			zap.String("container", w.container), zap.Error(err))
		return false
	}
	if !running {
		// The ML offline package isn't installed, or the user manually
		// stopped the container: don't take over, and don't spam logs.
		// The counter must be reset to zero: otherwise fails stays
		// permanently ≥ the threshold with lastRestart forever at its zero
		// value, forking a docker inspect on every 30s tick for the
		// process's entire lifetime on a machine without ML installed.
		// Resetting it brings the cadence back to once every failLimit ticks.
		w.fails = 0
		return false
	}
	zap.L().Warn("ml watchdog: backend unresponsive, restarting container",
		zap.String("container", w.container), zap.Int("consecutiveFails", w.fails))
	w.lastRestart = w.now()
	w.fails = 0
	if err := w.runner.Restart(ctx, w.container); err != nil {
		zap.L().Error("ml watchdog: docker restart failed", zap.Error(err))
		return false
	}
	zap.L().Info("ml watchdog: container restarted; embedder will backfill on recovery")
	return true
}

func (w *MLWatchdog) Run(ctx context.Context) {
	t := time.NewTicker(mlWatchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}
