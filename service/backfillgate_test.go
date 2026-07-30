package service

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBackfillGate_RunsFirstTriggerImmediately 验证 leading edge 不被延迟:
// 安静期后的第一次触发必须立刻执行,否则用户上传完照片要干等一个节流窗口
// 才看到 AI 索引/人物识别开始跑。
func TestBackfillGate_RunsFirstTriggerImmediately(t *testing.T) {
	g := newBackfillGate(time.Hour)
	var runs atomic.Int32

	g.Run("chain", func() { runs.Add(1) })

	require.Equal(t, int32(1), runs.Load(), "安静期后的首次触发必须同步立即执行")
}

// TestBackfillGate_CoalescesBurstIntoOneTrailingRun 是本次去抖的核心断言:
// 窗口内的连串触发只能合并成一次补跑(窗口末尾跑一次),不能每次都跑。
// 生产含义:批次完成钩子由「队列空闲 6 秒」触发,一块持续变动的素材盘会让它
// 每几秒触发一次全链补跑;去抖后每个窗口最多一轮。
func TestBackfillGate_CoalescesBurstIntoOneTrailingRun(t *testing.T) {
	g := newBackfillGate(150 * time.Millisecond)
	var runs atomic.Int32
	done := make(chan struct{}, 8)
	fn := func() {
		runs.Add(1)
		done <- struct{}{}
	}

	for i := 0; i < 5; i++ {
		g.Run("chain", fn)
	}
	<-done // leading edge
	require.Equal(t, int32(1), runs.Load(), "窗口内的后续触发不该立即执行")

	select {
	case <-done: // trailing edge
	case <-time.After(2 * time.Second):
		t.Fatal("窗口末尾应当补跑一次(触发不能被丢掉)")
	}
	require.Equal(t, int32(2), runs.Load(), "4 次窗口内触发只应合并成 1 次补跑")

	// 再等一个窗口,确认没有多余的尾随执行。
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int32(2), runs.Load(), "合并后的补跑只能有一次")
}

// TestBackfillGate_WindowsAreIndependentPerChain 验证窗口按链独立:一条链正
// 处于节流窗口内,不能把另一条链的首次触发也压到窗口末尾。
// 这正是实测踩到的坑——服务启动时 ML 恢复链先触发、吃掉共用的 leading edge,
// 紧随其后用户上传那一批的补跑就被推迟了整整一个窗口。
func TestBackfillGate_WindowsAreIndependentPerChain(t *testing.T) {
	g := newBackfillGate(time.Hour)
	var ranA, ranB atomic.Int32

	g.Run("chainA", func() { ranA.Add(1) }) // A 的 leading edge
	g.Run("chainA", func() { ranA.Add(1) }) // A 窗口内:应被推迟
	g.Run("chainB", func() { ranB.Add(1) }) // B 的首次触发:不该被 A 的窗口牵连

	require.Equal(t, int32(1), ranA.Load(), "同一条链窗口内的第二次触发应被推迟")
	require.Equal(t, int32(1), ranB.Load(), "另一条链的首次触发必须立即执行")
}

// TestBackfillGate_RunsAgainAfterWindowElapsed 验证节流窗口过后恢复立即执行,
// 不会一直把触发压成尾随。
func TestBackfillGate_RunsAgainAfterWindowElapsed(t *testing.T) {
	g := newBackfillGate(50 * time.Millisecond)
	var runs atomic.Int32

	g.Run("chain", func() { runs.Add(1) })
	time.Sleep(80 * time.Millisecond)
	g.Run("chain", func() { runs.Add(1) })

	require.Equal(t, int32(2), runs.Load(), "窗口过后的触发应恢复立即执行")
}

// TestEmbedder_ObserveReady_FirstReadyTriggersRecovery 验证启动时 ML 已就绪
// 也要触发一次恢复链(补齐上次运行期间积压的欠账)。
func TestEmbedder_ObserveReady_FirstReadyTriggersRecovery(t *testing.T) {
	e := NewEmbedder(makeTestDB(t), &mockML{}, nil, nil)
	require.True(t, e.observeReady(true), "首次探测到就绪必须触发恢复链")
	require.False(t, e.observeReady(true), "持续就绪不该重复触发")
}

// TestEmbedder_ObserveReady_SingleMissedPingDoesNotRetrigger 是 ML 抖动去抖的
// 核心断言:/ping 是 3 秒超时的探测,而 ML 冷加载/满负载时延迟实测能到 ~2.5s
// (见 mlwatchdog.go 的注释),偶发一次超时不代表掉线。若把它当掉线,负载一
// 回落就产生一次 false→true 跳变、触发整条恢复链,恢复链又把 ML 打满——这就
// 是生产上「ML 500/EOF 与补跑互相喂饱」的正反馈。
func TestEmbedder_ObserveReady_SingleMissedPingDoesNotRetrigger(t *testing.T) {
	e := NewEmbedder(makeTestDB(t), &mockML{}, nil, nil)
	require.True(t, e.observeReady(true))

	require.False(t, e.observeReady(false), "单次探测失败不该被当作掉线")
	require.False(t, e.observeReady(true), "单次抖动恢复后不该重跑恢复链")
}

// TestEmbedder_ObserveReady_ConsecutiveMissesThenReadyRetriggers 验证真掉线
// (连续多次探测失败)恢复后仍会触发恢复链——去抖不能把真故障也吞掉。
func TestEmbedder_ObserveReady_ConsecutiveMissesThenReadyRetriggers(t *testing.T) {
	e := NewEmbedder(makeTestDB(t), &mockML{}, nil, nil)
	require.True(t, e.observeReady(true))

	for i := 0; i < mlDownStreakThreshold; i++ {
		require.False(t, e.observeReady(false))
	}
	require.True(t, e.observeReady(true), "真掉线恢复后必须触发恢复链")
}
