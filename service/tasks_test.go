package service

import (
	"sync"
	"testing"
	"time"
)

func TestTaskRegistry_AddListRemove(t *testing.T) {
	r := NewTaskRegistry(nil)
	r.Upsert(Task{ID: "a", Type: "index", Label: "索引", Progress: 0.3, Status: "running", StartedAt: time.Now()})
	r.Upsert(Task{ID: "b", Type: "face", Label: "人脸", Progress: 0.1, Status: "running", StartedAt: time.Now()})

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(list))
	}
	r.Remove("a")
	if len(r.List()) != 1 {
		t.Fatalf("expected 1 task after remove, got %d", len(r.List()))
	}
}

func TestTaskRegistry_ConcurrentUpsert(t *testing.T) {
	r := NewTaskRegistry(nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Upsert(Task{ID: "a", Type: "index", Progress: float64(i) / 100.0, Status: "running"})
		}(i)
	}
	wg.Wait()
	if len(r.List()) != 1 {
		t.Fatalf("expected merged single task, got %d", len(r.List()))
	}
}

func TestTaskRegistry_ThrottlePublish(t *testing.T) {
	calls := 0
	pub := func(_ Task) { calls++ }
	r := NewTaskRegistry(pub)

	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.1, Status: "running"})
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.105, Status: "running"})
	if calls != 1 {
		t.Fatalf("expected 1 publish due to throttle, got %d", calls)
	}
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.12, Status: "running"})
	if calls != 2 {
		t.Fatalf("expected 2 publishes after 1%% cross, got %d", calls)
	}
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.12, Status: "done"})
	if calls != 3 {
		t.Fatalf("expected 3 publishes after status change, got %d", calls)
	}
}

func TestTaskRegistry_500msThrottleExpiry(t *testing.T) {
	calls := 0
	pub := func(_ Task) { calls++ }
	r := NewTaskRegistry(pub)

	// First upsert publishes immediately.
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.50, Status: "running"})
	if calls != 1 {
		t.Fatalf("expected 1 publish on first upsert, got %d", calls)
	}

	// Sub-bucket update within 500ms should be throttled out.
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.505, Status: "running"})
	if calls != 1 {
		t.Fatalf("expected throttle within 500ms, got %d publishes", calls)
	}

	// Wait past the throttle window. Same progress / same status —
	// only the time gap should trigger a re-publish.
	time.Sleep(550 * time.Millisecond)
	r.Upsert(Task{ID: "a", Type: "index", Progress: 0.505, Status: "running"})
	if calls != 2 {
		t.Fatalf("expected publish after 500ms expiry, got %d", calls)
	}
}
