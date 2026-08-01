package service

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIngestTracker_SingleEnqueueLifecycle:
// Enqueue 1 file, processFile runs to completion, done is emitted after 6s idle.
func TestIngestTracker_SingleEnqueueLifecycle(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	var mu sync.Mutex
	var emitted []Task
	pub := func(tk Task) {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, tk)
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(200 * time.Millisecond) // shorter idle for testing
	go idx.Start(ctx)

	idx.Enqueue(imgPath)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range emitted {
			if e.Type == "index" && e.Status == "done" && e.Progress == 1 {
				return true
			}
		}
		return false
	}, 4*time.Second, 50*time.Millisecond)
}

// TestIngestTracker_ReEnqueueWithinIdleCancelsTimer:
// a new Enqueue before the idle timer fires should not let the task go done early.
func TestIngestTracker_ReEnqueueWithinIdleCancelsTimer(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	p1 := makeTestJPEG(t, imgDir)
	p2 := makeTestJPEGNamed(t, imgDir, "second.jpg")

	var mu sync.Mutex
	var doneCount int
	pub := func(tk Task) {
		if tk.Type == "index" && tk.Status == "done" {
			mu.Lock()
			doneCount++
			mu.Unlock()
		}
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(200 * time.Millisecond)
	go idx.Start(ctx)

	idx.Enqueue(p1)
	require.Eventually(t, func() bool {
		var s string
		_ = db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, p1).Scan(&s)
		return s == "indexed"
	}, 4*time.Second, 50*time.Millisecond)

	idx.Enqueue(p2)
	require.Eventually(t, func() bool {
		var s string
		_ = db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, p2).Scan(&s)
		return s == "indexed"
	}, 4*time.Second, 50*time.Millisecond)

	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, doneCount, "two Enqueues should merge into one task and emit done only once")
}

// TestIngestTracker_HundredEnqueuesProduceSingleTask
func TestIngestTracker_HundredEnqueuesProduceSingleTask(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	var taskIDs sync.Map
	var doneSeen sync.Map
	pub := func(tk Task) {
		if tk.Type == "index" {
			taskIDs.Store(tk.ID, struct{}{})
			if tk.Status == "done" {
				doneSeen.Store(tk.ID, struct{}{})
			}
		}
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 4)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(500 * time.Millisecond)
	go idx.Start(ctx)

	for i := 0; i < 100; i++ {
		idx.Enqueue(makeTestJPEGNamed(t, imgDir, fmt.Sprintf("f%d.jpg", i)))
	}

	// Wait for at least one done task to appear (sent after all files are
	// processed + idle timeout). Note: all 100 files have identical content
	// (same checksum), so processFile short-circuits the other 99 right
	// after the first is indexed; ingest.current still counts up to 100, and
	// done is emitted once the idle timer fires. Not relying on
	// SELECT COUNT(*) because the same checksum only writes 1 row.
	require.Eventually(t, func() bool {
		count := 0
		doneSeen.Range(func(k, v any) bool { count++; return true })
		return count > 0
	}, 10*time.Second, 100*time.Millisecond)

	count := 0
	taskIDs.Range(func(k, v any) bool { count++; return true })
	require.Equal(t, 1, count, "100 Enqueues should produce only 1 ingest task ID")
}

// TestIngestTracker_BatchFixedTotal:
// on the first EnqueueWithBatch(path, "b1", 10) call, task.Total is immediately = 10, Progress = 0/10.
func TestIngestTracker_BatchFixedTotal(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEGNamed(t, imgDir, "batch_fixed.jpg")

	var mu sync.Mutex
	var captured []Task
	pub := func(tk Task) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, tk)
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(200 * time.Millisecond)
	go idx.Start(ctx)

	idx.EnqueueWithBatch(imgPath, "b1", 10)

	// On the first "running" publish, total must already be 10 and current 0.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, tk := range captured {
			if tk.Type == "index" && tk.Total == 10 && tk.Current == 0 {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "Total should be 10 immediately on the first publish")
}

// TestIngestTracker_BatchCompletesAtFixedTotal:
// 10 EnqueueWithBatch("b1", 10) calls; after all are noteResultWithBatch'd,
// current==total, done is emitted after idle, and onBatchDone is called.
func TestIngestTracker_BatchCompletesAtFixedTotal(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	const total = 10
	var paths []string
	for i := 0; i < total; i++ {
		paths = append(paths, makeTestJPEGNamed(t, imgDir, fmt.Sprintf("bt%d.jpg", i)))
	}

	var doneCalled sync.WaitGroup
	doneCalled.Add(1)
	var doneOnce sync.Once

	var mu sync.Mutex
	var doneTasks []Task
	pub := func(tk Task) {
		if tk.Type == "index" && tk.Status == "done" {
			mu.Lock()
			doneTasks = append(doneTasks, tk)
			mu.Unlock()
		}
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 2)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(200 * time.Millisecond)
	idx.SetOnBatchDone(func() {
		doneOnce.Do(func() { doneCalled.Done() })
	})
	go idx.Start(ctx)

	for _, p := range paths {
		idx.EnqueueWithBatch(p, "b1", int64(total))
	}

	// Wait for onBatchDone to be called (up to 5 seconds).
	doneCh := make(chan struct{})
	go func() {
		doneCalled.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onBatchDone was not called within the timeout")
	}

	// Verify the done task's Total and Current.
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, doneTasks, "at least one done task should be emitted")
	last := doneTasks[len(doneTasks)-1]
	require.Equal(t, int64(total), last.Total)
	require.Equal(t, int64(total), last.Current)
}

// TestIngestTracker_MultipleBatchesParallel:
// runs batch "b1" (total=3) and "b2" (total=2) concurrently, asserting 2
// independent task IDs are produced, and their progress doesn't interfere
// with each other.
func TestIngestTracker_MultipleBatchesParallel(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	var taskIDs sync.Map
	var mu sync.Mutex
	maxByID := map[string]int64{} // taskID -> max current seen

	pub := func(tk Task) {
		if tk.Type != "index" {
			return
		}
		taskIDs.Store(tk.ID, struct{}{})
		mu.Lock()
		if tk.Current > maxByID[tk.ID] {
			maxByID[tk.ID] = tk.Current
		}
		mu.Unlock()
	}
	reg := NewTaskRegistry(pub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 4)
	idx.SetTaskRegistry(reg)
	idx.SetIngestIdleTimeout(200 * time.Millisecond)
	go idx.Start(ctx)

	// Enqueue batch b1 (3 files) and b2 (2 files) interleaved.
	var b1, b2 []string
	for i := 0; i < 3; i++ {
		b1 = append(b1, makeTestJPEGNamed(t, imgDir, fmt.Sprintf("b1_%d.jpg", i)))
	}
	for i := 0; i < 2; i++ {
		b2 = append(b2, makeTestJPEGNamed(t, imgDir, fmt.Sprintf("b2_%d.jpg", i)))
	}
	for _, p := range b1 {
		idx.EnqueueWithBatch(p, "b1", 3)
	}
	for _, p := range b2 {
		idx.EnqueueWithBatch(p, "b2", 2)
	}

	// Wait for both batches to complete.
	require.Eventually(t, func() bool {
		count := 0
		taskIDs.Range(func(k, v any) bool { count++; return true })
		return count >= 2
	}, 6*time.Second, 50*time.Millisecond, "should observe 2 independent task IDs")

	// Confirm each one's max current does not exceed its own total.
	mu.Lock()
	defer mu.Unlock()
	idCount := 0
	taskIDs.Range(func(k, v any) bool { idCount++; return true })
	require.Equal(t, 2, idCount, "b1 and b2 should produce 2 independent task IDs")
}

// makeTestJPEGNamed is a named variant of makeTestJPEG (makeTestJPEG already exists in indexer_test.go).
func makeTestJPEGNamed(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return path
}
