package service

import (
	"sync"
	"time"
)

// Task is the single contract object exchanged via REST and MessageBus.
type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Label      string    `json:"label"`
	Current    int64     `json:"current,omitempty"`
	Total      int64     `json:"total,omitempty"`
	Progress   float64   `json:"progress"`
	Status     string    `json:"status"`
	ETASeconds int       `json:"eta_seconds,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Error      string    `json:"error,omitempty"`
}

// TaskPublisher emits task updates onto MessageBus.
type TaskPublisher func(Task)

// TaskRegistry holds running tasks and throttles publish.
type TaskRegistry struct {
	mu      sync.Mutex
	tasks   map[string]Task
	lastPub map[string]time.Time
	lastPct map[string]float64
	lastSt  map[string]string
	pub     TaskPublisher
}

// NewTaskRegistry creates an empty registry. `pub` may be nil.
func NewTaskRegistry(pub TaskPublisher) *TaskRegistry {
	return &TaskRegistry{
		tasks:   make(map[string]Task),
		lastPub: make(map[string]time.Time),
		lastPct: make(map[string]float64),
		lastSt:  make(map[string]string),
		pub:     pub,
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
