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
