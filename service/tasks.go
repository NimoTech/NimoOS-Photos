package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 任务错误的结构化 i18n key 契约:英文原文本身即 key(含 {参数} 占位),
// 前端按 key 查字典翻译、用 ErrorParams 填充占位;Error 字段只是英文 fallback。
// 新增/改动 key 文案需与前端 i18n 字典同步(前端另行维护，此处不引用)。
const (
	// TaskErrMLLostDuringBackfill 无参数。
	TaskErrMLLostDuringBackfill = "ML service was lost during backfill; please check the service status"
	// TaskErrOCRSourceReadFailed 参数: readFail。
	TaskErrOCRSourceReadFailed = "Source files for {readFail} photos could not be read; text recognition skipped"
	// TaskErrOCRBackfillFailed 参数: readFail, ocrFail。
	TaskErrOCRBackfillFailed = "Text recognition backfill failed (source read failed: {readFail}, ML failed: {ocrFail})"
	// TaskErrFaceClusterFailed 参数: detail。
	TaskErrFaceClusterFailed = "Face clustering failed: {detail}"
	// TaskErrPeopleRecognitionFailed 参数: detail。
	TaskErrPeopleRecognitionFailed = "People recognition failed: {detail}"
	// TaskErrQueryAssetsFailed 参数: detail。
	TaskErrQueryAssetsFailed = "Failed to query assets: {detail}"
	// TaskErrReadAssetListFailed 参数: detail。
	TaskErrReadAssetListFailed = "Failed to read asset list: {detail}"
)

// 任务停滞阈值:running 任务超过这么久没有任何更新,视为僵尸(漏发 done、
// goroutine 挂起、或声明总数>实际处理数导致 current 永远到不了 total),
// 由清扫器强制收尾。取值需明显大于任何单步最坏耗时(mlclient 单次 120s,
// 多模型串行最坏数百秒),5 分钟足够安全,不会误杀仍在缓慢推进的任务。
const taskStaleTimeout = 5 * time.Minute

// 清扫器扫描周期。
const taskSweepInterval = 1 * time.Minute

// Task is the single contract object exchanged via REST and MessageBus.
type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Label      string    `json:"label"`
	Current    int64     `json:"current,omitempty"`
	Total      int64     `json:"total,omitempty"`
	// Added 在终态(done)携带「本次新增数」语义:人脸聚类用它报本次新聚类(此前未分配)
	// 的人脸数,供前端「有新增才提示、且显示新增数而非总数」。0 表示无新增。
	Added      int64     `json:"added,omitempty"`
	Progress   float64   `json:"progress"`
	Status     string    `json:"status"`
	ETASeconds int       `json:"eta_seconds,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Error      string    `json:"error,omitempty"`
	// ErrorKey / ErrorParams 承载结构化 i18n 错误:ErrorKey 是上面契约里的英文原文
	// (含 {参数} 占位),ErrorParams 是占位替换值。Error 字段随之降级为英文 fallback,
	// 供没有走 i18n 字典的旧前端/日志兜底展示。均由 SetError 统一设置。
	ErrorKey    string            `json:"errorKey,omitempty"`
	ErrorParams map[string]string `json:"errorParams,omitempty"`
}

// SetError 设置结构化 i18n 错误(key + 参数),并生成英文 fallback 到 Error。
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
	lastSeen map[string]time.Time // 每个任务最后一次 Upsert 的时刻(不受发布节流影响),供停滞清扫器判活
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
	r.lastSeen[t.ID] = now // 任何更新都刷新活动时间(即便本次因节流未发布)
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

// StartStaleSweeper 启动一个后台清扫器(A:通用兜底)。它周期性扫描注册表,
// 把长时间(staleAfter)无更新、仍处于 "running" 的任务强制收尾为 "done" 并移除。
//
// 这是与任务类型无关的存活性保障:有 DB 真值的任务(index/ocr/clip)由前端按真值
// 对账即可自愈,但没有真值的任务(典型如人脸聚类——一次性内存计算,中途无可查询的
// 进度)一旦 goroutine 挂起或漏发 done,只有这里能兜底,避免注册表里出现永久僵尸。
// 阻塞直到 ctx 取消;通常以 go reg.StartStaleSweeper(ctx, ...) 方式调用。
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

// sweepStale 收集并清理停滞任务;now 作为参数注入以便测试。返回被清理的任务(便于测试断言)。
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
			"[taskRegistry] 清理停滞任务 id=%s type=%s label=%q last_progress=%.2f → 强制 done\n",
			f.ID, f.Type, f.Label, f.Progress)
		if pub != nil {
			pub(f)
		}
	}
	return stale
}
