package service

import (
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
