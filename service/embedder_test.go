package service

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func insertAsset(t *testing.T, db *sql.DB, path string, status string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
        INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
                           is_live_photo_video, status, checksum)
        VALUES(?,?,?, 'image/jpeg', ?, 0, ?, ?)`,
		id, path, 1, path, status, uuid.NewString())
	require.NoError(t, err)
	return id
}

func insertClipIdx(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)
}

// TestEmbedder_QueryMissing 只返回 status='indexed' 且 asset_clip_idx 没有行的 asset。
func TestEmbedder_QueryMissing(t *testing.T) {
	db := makeTestDB(t)
	missing := insertAsset(t, db, "/a.jpg", "indexed")
	_ = insertAsset(t, db, "/b.jpg", "pending") // 不该返回
	hasIdx := insertAsset(t, db, "/c.jpg", "indexed")
	insertClipIdx(t, db, hasIdx) // 已有 idx，不该返回

	e := NewEmbedder(db, &mockML{}, nil, nil)
	paths, err := e.queryMissing(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"/a.jpg"}, paths)
	_ = missing
}

// TestEmbedder_HasEmbeddingForPath
func TestEmbedder_HasEmbeddingForPath(t *testing.T) {
	db := makeTestDB(t)
	a := insertAsset(t, db, "/x.jpg", "indexed")
	insertClipIdx(t, db, a)
	_ = insertAsset(t, db, "/y.jpg", "indexed")

	e := NewEmbedder(db, &mockML{}, nil, nil)
	require.True(t, e.hasEmbeddingForPath("/x.jpg"))
	require.False(t, e.hasEmbeddingForPath("/y.jpg"))
	require.False(t, e.hasEmbeddingForPath("/nope.jpg"))
}

// flakyML：第 N 次 CLIPImageEmbed 返回 error，其余返回正常向量。
type flakyML struct {
	mockML
	failOnCalls map[int]bool
	calls       atomic.Int64
}

func (m *flakyML) CLIPImageEmbed(d []byte) ([]float32, error) {
	n := int(m.calls.Add(1))
	if m.failOnCalls[n] {
		return nil, fmt.Errorf("simulated ml failure")
	}
	return m.mockML.CLIPImageEmbed(d)
}

// makeUniqueJPEG 生成内容唯一的 JPEG（不同序号 → 不同 checksum），供 Backfill 测试使用。
// 与 makeTestJPEGNamed 的区别：填充 idx 颜色避免所有文件 checksum 相同。
func makeUniqueJPEG(t *testing.T, dir string, idx int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	c := color.RGBA{R: uint8(idx * 50 % 256), G: uint8(idx * 30 % 256), B: uint8(idx * 70 % 256), A: 255}
	for y := 0; y < 50; y++ {
		for x := 0; x < 50; x++ {
			img.Set(x, y, c)
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("u%d.jpg", idx))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return path
}

// TestEmbedder_Backfill_AllSuccess 5 个缺向量 → done, current=5
func TestEmbedder_Backfill_AllSuccess(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	// 用 indexer 先把图片 indexed-without-embedding：ML 不就绪跑一次
	idx := NewIndexer(db, &mockMLNotReady{}, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	var paths []string
	for i := 0; i < 5; i++ {
		p := makeUniqueJPEG(t, imgDir, i)
		paths = append(paths, p)
		idx.Enqueue(p)
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 5
	}, 10*time.Second, 100*time.Millisecond)

	// 现在用 ready ML 起 embedder
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, &mockML{}, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv, "应有 done event")
	require.Equal(t, int64(5), doneEv.Current)
	require.Equal(t, "生成 AI 索引", doneEv.Label)
}

// TestEmbedder_Backfill_PartialFail 3 成功 + 2 失败 → done, label 含"失败 2 张"
func TestEmbedder_Backfill_PartialFail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	for i := 0; i < 5; i++ {
		idx.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 5
	}, 10*time.Second, 100*time.Millisecond)

	flaky := &flakyML{failOnCalls: map[int]bool{2: true, 4: true}}
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, flaky, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, flaky, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var doneEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "done" {
			doneEv = &emitted[i]
		}
	}
	require.NotNil(t, doneEv)
	require.Contains(t, doneEv.Label, "失败 2 张")
}

// TestEmbedder_Backfill_AllFail 0 成功 + N 失败 → error
func TestEmbedder_Backfill_AllFail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go idx.Start(ctx)
	for i := 0; i < 3; i++ {
		idx.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 3
	}, 10*time.Second, 100*time.Millisecond)

	allFail := &flakyML{failOnCalls: map[int]bool{1: true, 2: true, 3: true}}
	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(t Task) { mu.Lock(); emitted = append(emitted, t); mu.Unlock() })
	idx2 := NewIndexer(db, allFail, thumbDir, 1)
	go idx2.Start(ctx)
	e := NewEmbedder(db, allFail, idx2, reg)
	require.NoError(t, e.Backfill(ctx))

	mu.Lock()
	defer mu.Unlock()
	var errEv *Task
	for i := range emitted {
		if emitted[i].Type == "embedding" && emitted[i].Status == "error" {
			errEv = &emitted[i]
		}
	}
	require.NotNil(t, errEv, "全失败应发 error event")
	require.Contains(t, errEv.Error, "ML")
}

// TestEmbedder_Backfill_CtxCancelMidwayDoesNotEmitDone:
// ctx 在循环中途被取消时，不应发 "done" 状态的 final task，且应返回 context.Canceled。
//
// 策略：
//   - 插 10 个真实 JPEG，让 Indexer 先把它们变成 status='indexed'（无 CLIP 向量）。
//   - 用 slowML（每次 CLIPImageEmbed 延迟 50ms）使整个循环需要 ~500ms。
//   - ctx 在 150ms 后超时，届时大约处理 2-3 个，break 触发。
//   - 修复前：break 后直接落入 final 决策 → 发 done；修复后：检查 ctx.Err() → return，不发 done。
func TestEmbedder_Backfill_CtxCancelMidwayDoesNotEmitDone(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	// 用 mockMLNotReady 先把图片 indexed（无 CLIP 向量）
	notReady := &mockMLNotReady{}
	idx0 := NewIndexer(db, notReady, thumbDir, 1)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go idx0.Start(bgCtx)
	for i := 0; i < 10; i++ {
		idx0.Enqueue(makeUniqueJPEG(t, imgDir, i))
	}
	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='indexed'`).Scan(&n)
		return n == 10
	}, 15*time.Second, 100*time.Millisecond, "等待 10 个 asset indexed")

	var emitted []Task
	var mu sync.Mutex
	reg := NewTaskRegistry(func(tk Task) { mu.Lock(); emitted = append(emitted, tk); mu.Unlock() })

	// slowML：每次 CLIPImageEmbed 延迟 50ms，10 个文件共需 ~500ms
	slow := &slowML{delay: 50 * time.Millisecond}
	idx2 := NewIndexer(db, slow, thumbDir, 1)
	go idx2.Start(bgCtx)
	e := NewEmbedder(db, slow, idx2, reg)

	// 150ms 后取消，届时循环只跑了 2-3 轮，尚未完成全部 10 个
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := e.Backfill(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "ctx 超时后应返回 DeadlineExceeded")

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range emitted {
		if ev.Type == "embedding" && ev.Status == "done" {
			t.Fatalf("不应在 ctx 取消后发 done event：%+v", ev)
		}
	}
}

// slowML 包装 mockML 给 CLIPImageEmbed 加固定延迟。
type slowML struct {
	mockML
	delay time.Duration
}

func (m *slowML) CLIPImageEmbed(d []byte) ([]float32, error) {
	time.Sleep(m.delay)
	return m.mockML.CLIPImageEmbed(d)
}

// TestEmbedder_Backfill_ConcurrencyGuard 同时调两次，第二个秒返回。
func TestEmbedder_Backfill_ConcurrencyGuard(t *testing.T) {
	db := makeTestDB(t)
	_ = insertAsset(t, db, "/a.jpg", "indexed") // 1 个缺向量

	e := NewEmbedder(db, &mockML{}, nil /* indexer 此用例不被调用 */, NewTaskRegistry(nil))

	e.running.Store(true)
	err := e.Backfill(context.Background())
	require.NoError(t, err, "已 running 时应秒返回 nil")
	e.running.Store(false)

	db2 := makeTestDB(t)
	e2 := NewEmbedder(db2, &mockML{}, nil, NewTaskRegistry(nil))
	require.NoError(t, e2.Backfill(context.Background()))
}
