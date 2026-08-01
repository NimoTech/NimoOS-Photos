package service

import (
	"context"
	"time"

	"go.uber.org/zap"
)

const (
	mlWatchdogInterval = 30 * time.Second
	// 12 ticks (~6 minutes): must be greater than
	// MACHINE_LEARNING_WORKER_TIMEOUT=300s. A cold model load blocks the
	// worker's event loop (measured /ping latency during load goes from
	// ~1ms to ~2.5s; a cold cache plus a slow disk can stall the heartbeat
	// for >120s), and /ping shares its heartbeat with gunicorn, so a slow
	// cold load can make /ping fail repeatedly without the worker actually
	// being wedged. The threshold is placed after gunicorn's own
	// self-healing window (kills and respawns the worker after a 300s
	// timeout), so the watchdog is strictly a second line of defense:
	// anything gunicorn can save never reaches us; only a worker still
	// unresponsive after 6 minutes is genuinely wedged.
	mlWatchdogFailLimit = 12
	mlWatchdogCooldown  = 10 * time.Minute
	mlContainerName     = "nimoos-photos-ml-immich-machine-learning-1"
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
