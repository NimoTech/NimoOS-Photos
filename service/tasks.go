package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// The structured i18n key contract for task errors: the English text itself
// is the key (with {param} placeholders); the frontend looks it up in its
// dictionary and fills placeholders with ErrorParams. The Error field is
// just the English fallback.
// New/changed key text must be kept in sync with the frontend's i18n
// dictionary (maintained separately, not referenced here).
const (
	// TaskErrMLLostDuringBackfill takes no params.
	TaskErrMLLostDuringBackfill = "ML service was lost during backfill; please check the service status"
	// TaskErrOCRSourceReadFailed param: readFail.
	TaskErrOCRSourceReadFailed = "Source files for {readFail} photos could not be read; text recognition skipped"
	// TaskErrOCRBackfillFailed params: readFail, ocrFail.
	TaskErrOCRBackfillFailed = "Text recognition backfill failed (source read failed: {readFail}, ML failed: {ocrFail})"
	// TaskErrFaceClusterFailed param: detail.
	TaskErrFaceClusterFailed = "Face clustering failed: {detail}"
	// TaskErrPeopleRecognitionFailed param: detail.
	TaskErrPeopleRecognitionFailed = "People recognition failed: {detail}"
	// TaskErrQueryAssetsFailed param: detail.
	TaskErrQueryAssetsFailed = "Failed to query assets: {detail}"
	// TaskErrReadAssetListFailed param: detail.
	TaskErrReadAssetListFailed = "Failed to read asset list: {detail}"
	// TaskErrPreviewFfmpegMissing takes no params.
	TaskErrPreviewFfmpegMissing = "ffmpeg is unavailable; video preview generation skipped"
	// TaskErrPreviewPartialFailed param: failed. Used only when every
	// candidate in this run failed to generate (matching BackfillOCR's
	// convention); when some fail but others still succeed, the task's
	// terminal state is done — the failure count is only logged, not routed
	// through this error key, to avoid a few corrupt videos repeatedly
	// flashing Failed.
	TaskErrPreviewPartialFailed = "Failed to generate previews for {failed} videos"
	// TaskErrMomentsRecomputeFailed param: detail. Covers only infrastructure
	// failures like the engine or persistence; LLM naming failure is
	// best-effort and silently skipped, not routed through this error key.
	TaskErrMomentsRecomputeFailed = "Smart moments recompute failed: {detail}"
)

// Task staleness threshold: a "running" task with no update for this long is
// considered a zombie (a missed "done" emission, a hung goroutine, or a
// declared total > actual processed count that keeps current from ever
// reaching total), and the sweeper force-finalizes it. The value must be
// clearly larger than any single step's worst-case duration (a single
// mlclient call is 120s, serial multi-model worst case is hundreds of
// seconds); 5 minutes is safely conservative without falsely killing tasks
// still making slow progress.
const taskStaleTimeout = 5 * time.Minute

// taskSweepInterval is the sweeper's scan period.
const taskSweepInterval = 1 * time.Minute

// Task is the single contract object exchanged via REST and MessageBus.
type Task struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	Current int64  `json:"current,omitempty"`
	Total   int64  `json:"total,omitempty"`
	// Added carries "count added this run" semantics in the terminal (done)
	// state: face clustering uses it to report the number of faces newly
	// clustered (previously unassigned) this run, so the frontend only shows
	// a hint when there's something new, displaying the added count rather
	// than the total. 0 means nothing added.
	Added      int64     `json:"added,omitempty"`
	Progress   float64   `json:"progress"`
	Status     string    `json:"status"`
	ETASeconds int       `json:"eta_seconds,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Error      string    `json:"error,omitempty"`
	// ErrorKey / ErrorParams carry the structured i18n error: ErrorKey is the
	// English text from the contract above (with {param} placeholders),
	// ErrorParams holds the placeholder substitution values. The Error field
	// then degrades to an English fallback, for older frontends/logs that
	// don't go through the i18n dictionary. Both are set uniformly by SetError.
	ErrorKey    string            `json:"errorKey,omitempty"`
	ErrorParams map[string]string `json:"errorParams,omitempty"`
}

// SetError sets a structured i18n error (key + params), and generates the
// English fallback into Error.
func (t *Task) SetError(key string, params map[string]string) {
	t.ErrorKey = key
	t.ErrorParams = params
	s := key
	for k, v := range params {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	t.Error = s
}

// TaskPublisher emits task updates onto MessageBus.
type TaskPublisher func(Task)

// TaskRegistry holds running tasks and throttles publish.
type TaskRegistry struct {
	mu       sync.Mutex
	tasks    map[string]Task
	lastPub  map[string]time.Time
	lastPct  map[string]float64
	lastSt   map[string]string
	lastSeen map[string]time.Time // time of each task's last Upsert (unaffected by publish throttling), used by the stale sweeper to judge liveness
	pub      TaskPublisher
}

// NewTaskRegistry creates an empty registry. `pub` may be nil.
func NewTaskRegistry(pub TaskPublisher) *TaskRegistry {
	return &TaskRegistry{
		tasks:    make(map[string]Task),
		lastPub:  make(map[string]time.Time),
		lastPct:  make(map[string]float64),
		lastSt:   make(map[string]string),
		lastSeen: make(map[string]time.Time),
		pub:      pub,
	}
}

// Upsert adds or updates a task. Publishes when:
//   - it's brand new
//   - status changed
//   - progress crossed a 1% bucket since last publish
//   - last publish was > 500ms ago
func (r *TaskRegistry) Upsert(t Task) {
	r.mu.Lock()
	r.tasks[t.ID] = t
	now := time.Now()
	r.lastSeen[t.ID] = now // any update refreshes the activity time (even if this one wasn't published due to throttling)
	prevPub, hadPub := r.lastPub[t.ID]
	prevPct := r.lastPct[t.ID]
	prevSt := r.lastSt[t.ID]
	shouldPublish := !hadPub ||
		t.Status != prevSt ||
		bucketCrossed(prevPct, t.Progress, 0.01) ||
		now.Sub(prevPub) >= 500*time.Millisecond
	if shouldPublish {
		r.lastPub[t.ID] = now
		r.lastPct[t.ID] = t.Progress
		r.lastSt[t.ID] = t.Status
	}
	pub := r.pub
	r.mu.Unlock()
	if shouldPublish && pub != nil {
		pub(t)
	}
}

// Remove deletes a task and its publish metadata.
func (r *TaskRegistry) Remove(id string) {
	r.mu.Lock()
	delete(r.tasks, id)
	delete(r.lastPub, id)
	delete(r.lastPct, id)
	delete(r.lastSt, id)
	delete(r.lastSeen, id)
	r.mu.Unlock()
}

// List returns a copy of current tasks.
func (r *TaskRegistry) List() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	return out
}

func bucketCrossed(prev, next, step float64) bool {
	if step <= 0 {
		return false
	}
	return int(next/step) > int(prev/step)
}

// StartStaleSweeper starts a background sweeper (A: generic fallback). It
// periodically scans the registry and force-finalizes any task still
// "running" with no update for staleAfter to "done", then removes it.
//
// This is a task-type-agnostic liveness guarantee: tasks backed by DB ground
// truth (index/ocr/clip) can self-heal via the frontend reconciling against
// that truth, but tasks with no ground truth (typically face clustering — a
// one-shot in-memory computation with no queryable progress midway) rely
// solely on this fallback if their goroutine hangs or misses emitting done,
// otherwise they'd sit as permanent zombies in the registry. Blocks until ctx
// is canceled; normally invoked as `go reg.StartStaleSweeper(ctx, ...)`.
func (r *TaskRegistry) StartStaleSweeper(ctx context.Context, staleAfter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweepStale(staleAfter, time.Now())
		}
	}
}

// sweepStale collects and cleans up stale tasks; now is injected as a
// parameter for testability. Returns the cleaned-up tasks (for test assertions).
func (r *TaskRegistry) sweepStale(staleAfter time.Duration, now time.Time) []Task {
	var stale []Task
	r.mu.Lock()
	for id, t := range r.tasks {
		if t.Status != "running" {
			continue
		}
		last, ok := r.lastSeen[id]
		if !ok {
			last = t.StartedAt
		}
		if now.Sub(last) <= staleAfter {
			continue
		}
		f := t
		f.Status = "done"
		f.Progress = 1
		stale = append(stale, f)
		delete(r.tasks, id)
		delete(r.lastPub, id)
		delete(r.lastPct, id)
		delete(r.lastSt, id)
		delete(r.lastSeen, id)
	}
	pub := r.pub
	r.mu.Unlock()

	for _, f := range stale {
		fmt.Fprintf(os.Stderr,
			"[taskRegistry] cleaning up stale task id=%s type=%s label=%q last_progress=%.2f → forcing done\n",
			f.ID, f.Type, f.Label, f.Progress)
		if pub != nil {
			pub(f)
		}
	}
	return stale
}
