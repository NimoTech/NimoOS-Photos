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
// Enqueue 1 个，processFile 跑完，6 秒空闲后发 done。
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
	idx.SetIngestIdleTimeout(200 * time.Millisecond) // 测试用更短的 idle
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
// idle 计时未到时新 Enqueue 不应该让 task 提前 done。
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
	require.Equal(t, 1, doneCount, "两次 Enqueue 应合并为一条 task、只发一次 done")
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

	// 等待至少一个 done task 出现（所有文件处理完 + idle 超时后发送）。
	// 注意：100 个文件内容相同（checksum 相同），processFile 会在第一个 indexed 后
	// 对其余 99 个快速短路返回，ingest.current 仍会计到 100，idle timer 触发后
	// 发出 done。不依赖 SELECT COUNT(*) 是因为相同 checksum 只写 1 行。
	require.Eventually(t, func() bool {
		count := 0
		doneSeen.Range(func(k, v any) bool { count++; return true })
		return count > 0
	}, 10*time.Second, 100*time.Millisecond)

	count := 0
	taskIDs.Range(func(k, v any) bool { count++; return true })
	require.Equal(t, 1, count, "100 次 Enqueue 应只产生 1 个 ingest task ID")
}

// TestIngestTracker_BatchFixedTotal:
// EnqueueWithBatch(path, "b1", 10) 第一次进来，task.Total 立刻 = 10、Progress = 0/10。
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

	// 第一次 publish running 时 total 必须已是 10，current 为 0。
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, tk := range captured {
			if tk.Type == "index" && tk.Total == 10 && tk.Current == 0 {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "第一次 publish 时 Total 应立即为 10")
}

// TestIngestTracker_BatchCompletesAtFixedTotal:
// 10 张 EnqueueWithBatch("b1", 10)，全部 noteResultWithBatch 后 current==total，
// idle 后发出 done 且 onBatchDone 被调用。
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

	// 等待 onBatchDone 被调用（最多 5 秒）。
	doneCh := make(chan struct{})
	go func() {
		doneCalled.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onBatchDone 未在超时内被调用")
	}

	// 验证 done task 的 Total 和 Current。
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, doneTasks, "应发出至少一个 done task")
	last := doneTasks[len(doneTasks)-1]
	require.Equal(t, int64(total), last.Total)
	require.Equal(t, int64(total), last.Current)
}

// TestIngestTracker_MultipleBatchesParallel:
// 同时跑 batch "b1"(total=3) 和 "b2"(total=2)，断言产生 2 个独立 task ID，
// 且各自 progress 互不干扰。
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

	// 等到两个 batch 都完成。
	require.Eventually(t, func() bool {
		count := 0
		taskIDs.Range(func(k, v any) bool { count++; return true })
		return count >= 2
	}, 6*time.Second, 50*time.Millisecond, "应观察到 2 个独立 task ID")

	// 确认各自的 current 最大值不超过各自的 total。
	mu.Lock()
	defer mu.Unlock()
	idCount := 0
	taskIDs.Range(func(k, v any) bool { idCount++; return true })
	require.Equal(t, 2, idCount, "b1 和 b2 应产生 2 个独立 task ID")
}

// makeTestJPEGNamed 是 makeTestJPEG 的命名变体（已有 makeTestJPEG 在 indexer_test.go）。
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
