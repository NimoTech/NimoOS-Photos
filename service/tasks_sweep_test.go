package service

import (
	"testing"
	"time"
)

// A stale running task should be force-finalized to done and removed by the sweeper.
func TestSweepStale_finalizesStuckRunningTask(t *testing.T) {
	var published []Task
	reg := NewTaskRegistry(func(t Task) { published = append(published, t) })

	start := time.Now()
	reg.Upsert(Task{ID: "index_1", Type: "index", Label: "Indexing photos",
		Current: 361, Total: 397, Progress: 0.91, Status: "running", StartedAt: start})

	// Simulate "still no update after 6 minutes".
	swept := reg.sweepStale(taskStaleTimeout, start.Add(6*time.Minute))

	if len(swept) != 1 {
		t.Fatalf("expected 1 swept task, got %d", len(swept))
	}
	if swept[0].Status != "done" || swept[0].Progress != 1 {
		t.Fatalf("swept task should be forced done/1, got status=%s progress=%v",
			swept[0].Status, swept[0].Progress)
	}
	if got := len(reg.List()); got != 0 {
		t.Fatalf("registry should be empty after sweep, got %d", got)
	}
	// A final done should be published (so the frontend can clear the zombie).
	if len(published) == 0 || published[len(published)-1].Status != "done" {
		t.Fatalf("expected a final done publish, got %+v", published)
	}
}

// A running task still actively progressing (recently updated) should not be falsely killed.
func TestSweepStale_keepsRecentlyUpdatedTask(t *testing.T) {
	reg := NewTaskRegistry(nil)
	start := time.Now()
	reg.Upsert(Task{ID: "face_1", Type: "face", Label: "Recognizing people",
		Progress: 0.5, Status: "running", StartedAt: start})

	// Only 1 minute has passed (< 5 minute threshold).
	swept := reg.sweepStale(taskStaleTimeout, start.Add(1*time.Minute))
	if len(swept) != 0 {
		t.Fatalf("recently-updated task must not be swept, got %d", len(swept))
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("task should still be present, got %d", got)
	}
}

// A task already in a terminal state (done/error) is not swept (handled by its own delayed cleanup).
func TestSweepStale_ignoresTerminalTasks(t *testing.T) {
	reg := NewTaskRegistry(nil)
	start := time.Now()
	reg.Upsert(Task{ID: "ocr_1", Type: "ocr", Progress: 1, Status: "done", StartedAt: start})

	swept := reg.sweepStale(taskStaleTimeout, start.Add(10*time.Minute))
	if len(swept) != 0 {
		t.Fatalf("terminal task must not be swept, got %d", len(swept))
	}
}
