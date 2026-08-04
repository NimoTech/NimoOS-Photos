package service

import (
	"sync"
	"time"
)

// defaultBackfillGateInterval is the per-chain throttle window for the heavy
// post-batch / ML-recovery backfill chains. A steadily-ingesting library
// fires the batch-done hook every few seconds (6s queue-idle); without the
// gate each firing re-runs the whole chain.
const defaultBackfillGateInterval = 10 * time.Minute

// backfillGate throttles named chains: the first trigger after quiet runs
// immediately (leading edge, synchronous — callers wrap in `go`); triggers
// inside the window are merged into exactly one trailing run that fires at
// the window's end and opens a fresh window. Chains are independent; the
// ML-recovery chain and the post-batch chain deliberately use different
// names so recovery can't eat the leading edge a user upload is waiting on.
type backfillGate struct {
	mu     sync.Mutex
	min    time.Duration
	chains map[string]*gatedChain
	now    func() time.Time
}

type gatedChain struct {
	lastRun   time.Time
	pendingFn func()
	timer     *time.Timer
}

func newBackfillGate(min time.Duration) *backfillGate {
	return &backfillGate{min: min, chains: map[string]*gatedChain{}, now: time.Now}
}

func (g *backfillGate) Run(name string, fn func()) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		c = &gatedChain{}
		g.chains[name] = c
	}
	now := g.now()
	// A pending trailing timer must force-merge regardless of the wall-clock
	// gap: c.timer is only cleared inside firePending's critical section,
	// which also refreshes lastRun in the same section. So observing
	// c.timer != nil here is strictly ordered before that clear — merging is
	// always correct. Without this check, a Run racing the timer's fire
	// could see a stale (pre-refresh) lastRun that already looks outside the
	// window, take the immediate branch, and run fn concurrently with the
	// about-to-fire trailing pendingFn — the exact double-run the gate
	// exists to prevent.
	if c.timer != nil || (!c.lastRun.IsZero() && now.Sub(c.lastRun) < g.min) {
		c.pendingFn = fn // merge: keep only the latest
		if c.timer == nil {
			c.timer = time.AfterFunc(g.min-now.Sub(c.lastRun), func() { g.firePending(name) })
		}
		g.mu.Unlock()
		return
	}
	c.lastRun = now
	g.mu.Unlock()
	fn()
}

func (g *backfillGate) firePending(name string) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		g.mu.Unlock()
		return
	}
	fn := c.pendingFn
	c.pendingFn = nil
	c.timer = nil
	c.lastRun = g.now()
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}
