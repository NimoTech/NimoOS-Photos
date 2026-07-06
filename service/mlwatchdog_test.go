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

// tickN 连续跑 n 次 Tick,返回累计触发的重启次数
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
	// 连续失败未达阈值(mlWatchdogFailLimit=12):不重启
	require.Equal(t, 0, tickN(w, 11))
	require.Equal(t, 0, r.restarts)
	// 第 12 次失败:触发重启
	require.True(t, w.Tick(context.Background()))
	require.Equal(t, 1, r.restarts)
}

func TestWatchdogResetsOnRecovery(t *testing.T) {
	r := &fakeRunner{running: true}
	ready := false
	w := NewMLWatchdog(func() bool { return ready }, r)
	tickN(w, 11)
	ready = true // 恢复
	require.False(t, w.Tick(context.Background()))
	ready = false // 再次失败,计数应从 0 重新累计
	require.Equal(t, 0, tickN(w, 11))
	require.Equal(t, 0, r.restarts)
}

func TestWatchdogSkipsWhenContainerNotRunning(t *testing.T) {
	r := &fakeRunner{running: false} // 未安装/被人为停止
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
	// 冷却期内继续失败:不再重启
	tickN(w, 24)
	require.Equal(t, 1, r.restarts)
	// 冷却期过后:允许再次重启
	now = now.Add(mlWatchdogCooldown + time.Second)
	tickN(w, 12)
	require.Equal(t, 2, r.restarts)
}

func TestWatchdogToleratesRestartError(t *testing.T) {
	r := &fakeRunner{running: true, restartErr: context.DeadlineExceeded}
	w := NewMLWatchdog(func() bool { return false }, r)
	require.Equal(t, 0, tickN(w, 12)) // Restart 报错时 Tick 返回 false,不 panic
	require.Equal(t, 1, r.restarts)   // 但确实尝试过一次
}

func TestWatchdogInspectCadenceWhenContainerNotRunning(t *testing.T) {
	r := &fakeRunner{running: false}
	w := NewMLWatchdog(func() bool { return false }, r)
	// 36 个 tick(18 分钟):只应在第 12/24/36 次各 inspect 一次,
	// 而不是 fails 卡在阈值上后每个 tick 都 fork docker inspect
	require.Equal(t, 0, tickN(w, 36))
	require.Equal(t, 3, r.inspects)
	require.Equal(t, 0, r.restarts)
}
