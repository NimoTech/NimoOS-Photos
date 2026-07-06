package service

import (
	"context"
	"time"

	"go.uber.org/zap"
)

const (
	mlWatchdogInterval = 30 * time.Second
	// 12 次(≈6 分钟):必须大于 MACHINE_LEARNING_WORKER_TIMEOUT=300s。
	// 模型冷加载会阻塞 worker 事件循环(实测加载期间 /ping 延迟 ~1ms→~2.5s;
	// 冷缓存+低端盘时可拖停心跳 >120s),/ping 与 gunicorn 心跳同源,慢冷载
	// 会让 /ping 连续失败但 worker 并没有卡死。阈值放到 gunicorn 自愈窗口
	// (300s 超时杀 worker 重拉)之后,看门狗严格作为第二道防线:
	// gunicorn 能救的轮不到我们;6 分钟还不应答的才是真卡死。
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
		// ML 离线包未安装、或用户手动停了容器:不接管,也不刷屏。
		// 必须清零计数:否则 fails 常驻 ≥阈值 且 lastRestart 恒为零值,
		// 此后每个 30s tick 都会 fork 一次 docker inspect,在没装 ML 的
		// 机器上持续整个进程生命周期。清零后节奏回到每 failLimit 个 tick 一次。
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
