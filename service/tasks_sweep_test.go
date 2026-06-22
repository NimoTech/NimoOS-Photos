package service

import (
	"testing"
	"time"
)

// 停滞的 running 任务应被清扫器强制收尾为 done 并移除。
func TestSweepStale_finalizesStuckRunningTask(t *testing.T) {
	var published []Task
	reg := NewTaskRegistry(func(t Task) { published = append(published, t) })

	start := time.Now()
	reg.Upsert(Task{ID: "index_1", Type: "index", Label: "索引照片",
		Current: 361, Total: 397, Progress: 0.91, Status: "running", StartedAt: start})

	// 模拟“6 分钟后仍无任何更新”。
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
	// 末尾应发布一条 done(让前端清掉僵尸)。
	if len(published) == 0 || published[len(published)-1].Status != "done" {
		t.Fatalf("expected a final done publish, got %+v", published)
	}
}

// 仍在活跃推进(最近有更新)的 running 任务不应被误杀。
func TestSweepStale_keepsRecentlyUpdatedTask(t *testing.T) {
	reg := NewTaskRegistry(nil)
	start := time.Now()
	reg.Upsert(Task{ID: "face_1", Type: "face", Label: "识别人物",
		Progress: 0.5, Status: "running", StartedAt: start})

	// 只过了 1 分钟(< 5 分钟阈值)。
	swept := reg.sweepStale(taskStaleTimeout, start.Add(1*time.Minute))
	if len(swept) != 0 {
		t.Fatalf("recently-updated task must not be swept, got %d", len(swept))
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("task should still be present, got %d", got)
	}
}

// 已是终态(done/error)的任务不参与清扫(由各自的延时清理负责)。
func TestSweepStale_ignoresTerminalTasks(t *testing.T) {
	reg := NewTaskRegistry(nil)
	start := time.Now()
	reg.Upsert(Task{ID: "ocr_1", Type: "ocr", Progress: 1, Status: "done", StartedAt: start})

	swept := reg.sweepStale(taskStaleTimeout, start.Add(10*time.Minute))
	if len(swept) != 0 {
		t.Fatalf("terminal task must not be swept, got %d", len(swept))
	}
}
