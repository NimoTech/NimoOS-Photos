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
//
// Guarantee: at most one execution per chain at any time; triggers during
// execution or within the window merge into one deferred run.
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
	// inFlight is true for the duration of an execution (leading edge or
	// deferred). Its purpose is to guarantee at most one execution per chain
	// at any time: a fn that runs longer than the window must not let a
	// fresh Run take the immediate branch and overlap it. While inFlight,
	// every trigger merges into pendingFn instead; the completion step
	// (see complete) is solely responsible for scheduling what runs next.
	inFlight bool
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
	// A pending trailing timer, or an in-flight execution, must force-merge
	// regardless of the wall-clock gap: c.timer is only cleared inside
	// firePending's critical section, which also refreshes lastRun in the
	// same section; c.inFlight is only cleared inside complete's critical
	// section, which is the sole place that arms a fresh timer. So observing
	// c.timer != nil or c.inFlight here is strictly ordered before those
	// transitions — merging is always correct. Without this check, a Run
	// racing the timer's fire (or a long-running fn) could see a stale
	// (pre-refresh) lastRun that already looks outside the window, take the
	// immediate branch, and run fn concurrently with the about-to-fire
	// trailing pendingFn or the still-executing leading run — the exact
	// double-run the gate exists to prevent.
	withinWindow := !c.lastRun.IsZero() && now.Sub(c.lastRun) < g.min
	if c.inFlight || c.timer != nil || withinWindow {
		c.pendingFn = fn // merge: keep only the latest
		// Arm the timer only when nothing is already going to schedule the
		// next run: never while inFlight (a negative-delay AfterFunc would
		// fire immediately and overlap the still-running execution —
		// complete's completion step takes over scheduling once it ends),
		// and only inside the window (outside it, with nothing in flight
		// and no timer, Run would have taken the immediate branch instead).
		if c.timer == nil && !c.inFlight && withinWindow {
			c.timer = time.AfterFunc(g.min-now.Sub(c.lastRun), func() { g.firePending(name) })
		}
		g.mu.Unlock()
		return
	}
	c.lastRun = now
	c.inFlight = true
	g.mu.Unlock()
	fn()
	g.complete(name)
}

// complete runs after a leading-edge or deferred execution finishes. It
// clears inFlight and, if a trigger merged into pendingFn while the
// execution was running, arms the timer that will fire it — the only place
// (besides Run's within-window leading-edge-adjacent arm) that schedules a
// pending run, guaranteeing the gate never needs a negative-delay timer
// racing an in-flight execution.
func (g *backfillGate) complete(name string) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		g.mu.Unlock()
		return
	}
	c.inFlight = false
	if c.pendingFn != nil && c.timer == nil {
		delay := g.min - g.now().Sub(c.lastRun)
		if delay < 0 {
			delay = 0
		}
		c.timer = time.AfterFunc(delay, func() { g.firePending(name) })
	}
	g.mu.Unlock()
}

func (g *backfillGate) firePending(name string) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		g.mu.Unlock()
		return
	}
	if c.inFlight {
		// Defensive: shouldn't occur, since Run and complete never arm a
		// timer while inFlight is true. If it ever did, leave pendingFn in
		// place — the completion step of the in-flight execution will
		// re-arm once it finishes.
		c.timer = nil
		g.mu.Unlock()
		return
	}
	fn := c.pendingFn
	c.pendingFn = nil
	c.timer = nil
	if fn == nil {
		g.mu.Unlock()
		return
	}
	c.lastRun = g.now()
	c.inFlight = true
	g.mu.Unlock()
	fn()
	g.complete(name)
}
