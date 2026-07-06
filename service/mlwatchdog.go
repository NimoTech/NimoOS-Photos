package service

import (
	"context"
	"time"

	"go.uber.org/zap"
)

const (
	mlWatchdogInterval  = 30 * time.Second
	mlWatchdogFailLimit = 4 // 连续 4 次 /ping 失败(≈2 分钟)才动手
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
// compose 的 restart:unless-stopped 只救「进程退出」,救不了这种 hang。
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
		// ML 离线包未安装、或用户手动停了容器:不接管,也不刷屏
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
