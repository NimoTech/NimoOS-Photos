package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackfillGate_RunsFirstTriggerImmediately(t *testing.T) {
	g := newBackfillGate(time.Hour)
	var ran atomic.Int32
	g.Run("a", func() { ran.Add(1) })
	require.EqualValues(t, 1, ran.Load()) // leading edge is synchronous, never deferred
}

func TestBackfillGate_CoalescesBurstIntoOneTrailingRun(t *testing.T) {
	g := newBackfillGate(150 * time.Millisecond)
	var ran atomic.Int32
	done := make(chan struct{}, 8)
	fn := func() { ran.Add(1); done <- struct{}{} }
	for i := 0; i < 5; i++ {
		g.Run("a", fn)
	}
	require.EqualValues(t, 1, ran.Load()) // burst: exactly one immediate run
	select {                              // ...then exactly one trailing run
	case <-done:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("trailing run never fired")
	}
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 2, ran.Load()) // and nothing extra after that
}

func TestBackfillGate_WindowsAreIndependentPerChain(t *testing.T) {
	g := newBackfillGate(time.Hour)
	var ranA, ranB atomic.Int32
	g.Run("a", func() { ranA.Add(1) })
	g.Run("b", func() { ranB.Add(1) })
	require.EqualValues(t, 1, ranA.Load())
	require.EqualValues(t, 1, ranB.Load()) // B's first trigger not held hostage by A's window
}

func TestBackfillGate_RunsAgainAfterWindowElapsed(t *testing.T) {
	g := newBackfillGate(50 * time.Millisecond)
	var ran atomic.Int32
	g.Run("a", func() { ran.Add(1) })
	time.Sleep(80 * time.Millisecond)
	g.Run("a", func() { ran.Add(1) })
	require.EqualValues(t, 2, ran.Load())
}

// TestBackfillGate_NoConcurrentRunsAcrossWindowBoundary reproduces the
// logic race where a fresh Run(name, fn2) races the trailing timer's
// firePending goroutine for g.mu right at the window boundary: without the
// "armed timer forces merge" check, Run can observe a stale (pre-refresh)
// lastRun that already looks outside the window, take the immediate branch,
// and run fn concurrently with the about-to-fire trailing pendingFn. This is
// a pure interleaving/logic race — the data itself (an atomic gauge) is
// race-detector-clean either way, so -race alone can't catch it; only the
// concurrent-invocation count can.
//
// The exact race is a narrow timing window (the instant now.Sub(lastRun)
// crosses g.min while firePending's goroutine is separately contending for
// g.mu), so the stress needs to cross many window boundaries to have a
// realistic chance of landing in it. An unbounded `select { default: ... }`
// busy spin against a single mutex was tried first and observed to make
// forward progress pathologically slowly under -race (goroutines starved on
// g.mu for minutes) without ever exercising more than one window cycle — not
// the interleaving under test. Pacing each call with a tiny sleep instead
// avoids the livelock while still crossing dozens of ~20ms window boundaries
// per run.
func TestBackfillGate_NoConcurrentRunsAcrossWindowBoundary(t *testing.T) {
	g := newBackfillGate(20 * time.Millisecond)
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	fn := func() {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(500 * time.Microsecond) // widen the overlap window so a double-run is observable
		inFlight.Add(-1)
	}

	deadline := time.Now().Add(400 * time.Millisecond)
	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				g.Run("a", fn)
				time.Sleep(50 * time.Microsecond) // pace to avoid a CPU-pinned spin under -race
			}
		}()
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond) // let any remaining trailing timer drain

	require.LessOrEqual(t, maxInFlight.Load(), int32(1), "concurrent invocations of the same chain observed")
}

// TestBackfillGate_NoOverlapWhenRunOutlivesWindow reproduces the bug this
// fix addresses: when fn runs longer than the window, a fresh Run(name, ...)
// that arrives after the window has elapsed — but while the leading fn is
// still executing — must not take the immediate branch. Before inFlight
// existed, it would: Run only checked "outside the window" against
// lastRun, which is set at the start of the execution, so once the window
// elapsed mid-run every subsequent Run call ran fn concurrently with the
// still-in-flight leading call (observed as maxInFlight == 2+).
//
// The burst below fires calls both inside and past the window boundary
// while the leading call sleeps for ~3x the window, then asserts the whole
// burst merged into pendingFn (maxInFlight never exceeds 1) and that exactly
// one deferred run — the merged pending trigger, armed by the completion
// step once the leading call finally releases inFlight — executes
// afterwards.
func TestBackfillGate_NoOverlapWhenRunOutlivesWindow(t *testing.T) {
	window := 30 * time.Millisecond
	g := newBackfillGate(window)

	var inFlightN atomic.Int32
	var maxInFlight atomic.Int32
	var runs atomic.Int32
	fn := func() {
		n := inFlightN.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(3 * window) // outlive the window: fn runs ~3x longer than the gate's throttle window
		runs.Add(1)
		inFlightN.Add(-1)
	}

	leadingDone := make(chan struct{})
	go func() {
		g.Run("a", fn)
		close(leadingDone)
	}()
	time.Sleep(2 * time.Millisecond) // let the leading call enter fn (set inFlight) before the burst starts

	// Burst of Run calls while the leading call is still executing, spanning
	// both inside and past the window boundary — the exact scenario that
	// used to double-run.
	burstDeadline := time.Now().Add(2 * window)
	for time.Now().Before(burstDeadline) {
		g.Run("a", fn)
		time.Sleep(2 * time.Millisecond)
	}

	<-leadingDone
	require.Eventually(t, func() bool { return runs.Load() == 2 }, 2*time.Second, 5*time.Millisecond,
		"exactly one deferred run (the merged pending trigger) should execute after the leading run finishes")

	require.EqualValues(t, 1, maxInFlight.Load(), "no two invocations of the same chain should overlap")
	time.Sleep(3 * window)
	require.EqualValues(t, 2, runs.Load(), "leading run + exactly one merged deferred run, nothing extra")
}
