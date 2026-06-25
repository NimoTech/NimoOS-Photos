# 视频悬停预览（Sprite）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 视频 tile 悬停时显示一张 sprite 雪碧图，鼠标左右移动即可 scrub 不同时间段；sprite 在首次请求时按需生成并永久缓存。

**Architecture:** 纯懒加载。入库流水线零改动。新增 `GET /assets/:id/sprite`：缓存命中直接返回；未命中时在请求 goroutine 上用 ffmpeg 同步生成（全局信号量限 2 + per-asset 去重 + 临时文件原子 rename），落盘到 `<thumbDir>/<id>/sprite.jpg`。前端用 `background-position-x` 在单帧窗口里切换显示，帧↔时间为纯线性公式。

**Tech Stack:** Go (Echo v4, CGO+sqlite3), ffmpeg/ffprobe CLI, Vue 2 + vitest。

---

> ## 2026-06-23 修订：前端重写为 tile 内覆盖层 + 竞态修复
>
> 下方 Task 1–5（后端）保持有效。**Task 6/7 的前端代码块为旧版（tile 上方 `position:fixed` 浮窗，且 `onVideoEnter` 无令牌守卫、含「悬停 A 却预览 B」竞态 bug），现已被以下实现取代**（spec §5 已在 06-22/06-23 同步重写）：
>
> - **`hoverScrub.js`**：新增纯函数 `computeCoverStyle(boxW, boxH, frameCount, frameW, frameH, currentFrame)` —— 把单帧 cover 缩放填满任意密度的 tile，返回 `{backgroundSize, backgroundPositionX}`；盒子未测量（0×0）时回退 1:1。
> - **`spritePreview.js`（新文件）**：`async loadSpriteMeta(url, fetchFn, isCurrent)` —— 发起 `/sprite` 请求并用 `isCurrent()`（闭包捕获 hoverToken）守卫：`await` 后若令牌已变返回 `{stale:true}` 且不带元数据，调用方据此丢弃、绝不回写共享状态。**这是竞态修复的核心。**
> - **`VideoHoverPreview.vue`**：由浮窗改为 `position:absolute; inset:0; z-index:1` 的 tile 内覆盖层；`ResizeObserver`/`clientWidth` 测盒子尺寸后用 `computeCoverStyle` 缩放当前帧；底部细进度条；右下角小号淡色当前时间文字（去掉 `x`/`y` 与「/ 总时长」，保留 `durationMs`）。
> - **`PhotosGrid.vue`**：`<VideoHoverPreview>` 改挂到视频 tile 内部（`v-if="hoveredVideo===p"`）；`data` 去掉 `previewPos`、新增 `hoverToken`；`onVideoEnter` 用 `loadSpriteMeta` + 令牌守卫，`onVideoLeave` 自增 `hoverToken` invalidate 在途请求。
> - **测试**：`tests/photosGridHover.test.js`（+`computeCoverStyle`）、`tests/videoHoverPreview.test.js`（改写为覆盖层断言）、`tests/videoHoverPreviewRace.test.js`（新增竞态回归：慢 A + 快 B，断言过期 A 不污染当前 spriteUrl/frameCount）。全部 TDD（先红后绿），全量 744 测试通过、`vue-cli-service lint` 无错。
>
> 仍待人工 E2E（见 Task 8 Step 1–3，未勾选）。
>
> ### 2026-06-23 追加（视觉/密度调优）
>
> 真机预览后两处反馈：
> - **帧未居中** → `hoverScrub.js` 的 `computeCoverStyle` 改名/改算法为 `computeFrameStyle`：由 `cover`（`s=max`，左对齐裁切）改为 **`contain`（`s=min`，整帧可见 + 水平/垂直居中）**，`backgroundPositionX/Y` 均带居中项；`VideoHoverPreview.vue` 的 `.sprite-window` 加 `background-color:#000` 提供黑边，并把帧层/进度条/时间文字 gate 在 `spriteUrl` 上（就绪前透出静态缩略图）。测试：`photosGridHover.test.js`、`videoHoverPreview.test.js` 改为 contain 断言 + 新增 fallback 断言。
> - **帧数太低** → `service/sprite.go`：`SpriteFrameCount` 由 `clamp(durS/30, 5, 20)` 改为 **`clamp(durS/10, 10, 40)`**（密度翻倍）。测试：`service/sprite_test.go` `TestSpriteFrameCountClamp`、`TestEnsureSkipsWhenExists`、`route/v1/assets_sprite_test.go` `TestSpriteGeneratesAndServes`（6s→10 帧）同步更新。
> - **部署注意**：sprite 是永久缓存，改帧数公式**不会**自动重生已有 `sprite.jpg`。需 `sudo systemctl stop nimoos-photos`、清缓存（`find <thumbDir> -name sprite.jpg -delete`，`thumbDir=/DATA/.system_data/photos/thumbs`）、装新二进制、再 start，旧视频才会按新帧数重生。
>
> ### 2026-06-23 再追加（原始比例 + 提画质）
>
> 反馈"视频要铺满（横向先满/竖向先满）、提画质"。诊断：旧方案后端把所有视频强制 `pad` 成固定 16:9 120×68，竖屏视频被压成横条 + 前端再 contain → 双重黑边、画面小。修复：
> - **`pkg/ffmpeg/ffmpeg.go`**：vf 由 `scale=120:68:force_original_aspect_ratio=decrease,pad=120:68:-1:-1,tile=Nx1` 改为 **`scale=240:-2,tile=Nx1`**（固定宽 240、高按原始比例自适应、**去掉 pad**）；`-q:v 6 → 4`。
> - **`service/sprite.go`**：`SpriteFrameW 120→240`、`SpriteFrameH 68→135`（名义默认）；新增 `SpriteInfoFromFile`（同时返回帧数 + 实际帧高），`SpriteFrameCountFromFile` 改为其薄封装。
> - **`route/v1/assets.go`**：handler 用 `SpriteInfoFromFile`，`X-Sprite-Frame-H` 返回**实际帧高**（不再是常量）。
> - **前端**：`spritePreview.js` 的 `loadSpriteMeta` 多解析 `X-Sprite-Frame-W/H` 并返回；`PhotosGrid.vue` 新增 `spriteFrameW/spriteFrameH` data、enter 时从响应写入、`<VideoHoverPreview :frame-w/:frame-h>` 改绑这两个；组件 prop 默认 `240/135`。`computeFrameStyle`（contain 居中）天然适配任意帧宽高，无需改。
> - **测试**：`ffmpeg_test`（320×240 源 → 240×180）、`sprite_test`（新增 `TestSpriteInfoFromFile`）、UI `videoHoverPreviewRace`（响应头加 frame-w/h）同步更新；后端 `./service ./route/v1 ./pkg/ffmpeg` 全绿、UI 745/745、lint 无错。
> - **效果**：横屏铺满宽（上下黑边）、竖屏铺满高（左右黑边），单方向"先到边先满"；画质 2× + q4。同样需重新部署 + 清 sprite 缓存（同上）。
>
> ### 2026-06-24 追加（帧数策略改为分档降密度）
>
> 反馈"想要一秒一帧、中长视频帧数递减、设最大帧数、最小值 5"。改 `service/sprite.go` 的 `SpriteFrameCount`：由 `clamp(durS/10, 10, 40)` 改为分档——`≤30s→1帧/s`、`30–120s→1帧/2s`、`>120s→1帧/4s`，再 clamp `[5, 120]`。常量 `spriteMinFrames 10→5`、`spriteMaxFrames 40→120`，删除 `spriteSecsPerFrm`。
> - **封顶 120 的依据**：sprite 单张 JPEG 宽 = 帧数×240px，120 帧 = 28800px（远低于 JPEG ~65535px 上限）；tile 屏上仅约 200px 宽，帧数过 ~200 后鼠标无法再细分，120 已足够丝滑。
> - **测试**：`service/sprite_test.go` `TestSpriteFrameCountClamp`（改写为分档断言）、`TestEnsureSkipsWhenExists`（时长 100s→10s 以匹配 10 帧假文件）、`route/v1/assets_sprite_test.go` `TestSpriteGeneratesAndServes`（6s→6 帧）同步更新；`./service ./route/v1 ./pkg/ffmpeg` 全绿。前端无需改（`frameCount` 从 `X-Sprite-Frames` 响应头动态读）。
> - **部署注意**：与历次一样，sprite 永久缓存，改帧数公式**不会**自动重生已有 `sprite.jpg`。需 stop → 清缓存（`find /DATA/.system_data/photos/thumbs -name sprite.jpg -delete`）→ 装新二进制 → start。

## Global Constraints

- 设计依据：`docs/superpowers/specs/2026-06-18-video-hover-preview-design.md`（只做 Layer 1；视频语义检索不在范围内）。
- 帧规格：单帧 **120×68**，JPEG `-q:v 6`，横向 `tile=Nx1`。
- 帧数（2026-06-24 改为分档降密度，见下方追加修订；原 `clamp(durS/30, 5, 20)`）：`≤30s→1帧/s，30–120s→1帧/2s，>120s→1帧/4s`，clamp `[5, 120]`。
- ffmpeg 滤镜用 **`fps=(N+1)/D` 过采样 + `-frames:v 1`**；`D`（秒）必须 > 0。**绝不**用 `-vframes` 控制帧数（它是输出选项，进 `-vf` 会报错）。
- 全局并发：同时运行的 sprite ffmpeg 进程上限 **2**。
- CGO 必需：`CGO_ENABLED=1 go build/test`（仓库已默认）。
- 标准响应/错误沿用现有 handler 风格（`echo.NewHTTPError`）。
- 删除与 prune **无需新增代码**：`trash.go:175`、`indexer.go:1158/1196` 整目录 `RemoveAll(thumbDir/<id>)`；`storage.go:176-186` 的 Prune 只删整个孤儿 `<id>` 目录，从不删有效资产目录内的单个文件。sprite.jpg 住在 `<id>/` 内，自然被覆盖。

---

### Task 1: ffmpeg.GenerateSprite（纯命令封装）

**Files:**
- Modify: `pkg/ffmpeg/ffmpeg.go`（在 `ExtractKeyframe` 之后新增函数）
- Test: `pkg/ffmpeg/ffmpeg_test.go`

**Interfaces:**
- Produces: `func GenerateSprite(videoPath, outPath string, frameCount int, durationS float64) error`
  - 把视频均匀采样 `frameCount` 帧，拼成单张横向 sprite JPEG（每格 120×68）写到 `outPath`。
  - 先写临时文件再原子 `os.Rename`，避免半成品/并发写损坏。
  - `durationS<=0` 或 `frameCount<1` 直接返回 error（调用方保证 >0）。
  - ffmpeg 不在 PATH 时返回的 error 链包含 `exec.ErrNotFound`（用 `%w` 透传）。

- [x] **Step 1: 写失败测试**

在 `pkg/ffmpeg/ffmpeg_test.go` 末尾追加：

```go
func TestGenerateSpriteProducesTile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH")
	}
	dir := t.TempDir()
	// 造一个 6 秒测试视频
	src := filepath.Join(dir, "src.mp4")
	mk := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=25", "-y", src)
	require.NoError(t, mk.Run())

	out := filepath.Join(dir, "sub", "sprite.jpg") // 子目录不存在，验证自动建目录
	require.NoError(t, ffmpeg.GenerateSprite(src, out, 10, 6.0))

	f, err := os.Open(out)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err)
	require.Equal(t, 10*120, cfg.Width) // tile=10x1 → 宽恒为 N*120
	require.Equal(t, 68, cfg.Height)
}

func TestGenerateSpriteRejectsZeroDuration(t *testing.T) {
	require.Error(t, ffmpeg.GenerateSprite("/any.mp4", "/tmp/x.jpg", 10, 0))
}
```

在 `ffmpeg_test.go` 的 import 块补上 `"os"`, `"path/filepath"`, `"image"`, `_ "image/jpeg"`（`os/exec`、`testing`、`require` 已有）。

- [x] **Step 2: 运行测试，确认失败**

Run: `CGO_ENABLED=1 go test ./pkg/ffmpeg/ -run TestGenerateSprite -v`
Expected: 编译失败 `undefined: ffmpeg.GenerateSprite`。

- [x] **Step 3: 实现 GenerateSprite**

在 `pkg/ffmpeg/ffmpeg.go` 新增（确保文件已 import `os/exec`、`fmt`、`os`、`path/filepath`——前两者已有，按需补 `os`/`path/filepath`，它们也已存在）：

```go
// GenerateSprite extracts frameCount evenly-spaced frames from videoPath and
// writes them as a single horizontal sprite JPEG (each cell 120x68) to outPath.
// durationS must be > 0. It oversamples by one frame (fps=(N+1)/D) so the tile
// is always full — never use -vframes to bound the count (it is an output
// option and cannot live inside -vf). The image is written to a temp file and
// atomically renamed, so concurrent generations and crashes never leave a
// partial sprite.
func GenerateSprite(videoPath, outPath string, frameCount int, durationS float64) error {
	if durationS <= 0 {
		return fmt.Errorf("GenerateSprite: durationS must be > 0, got %v", durationS)
	}
	if frameCount < 1 {
		return fmt.Errorf("GenerateSprite: frameCount must be >= 1, got %d", frameCount)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("GenerateSprite: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".sprite-*.jpg")
	if err != nil {
		return fmt.Errorf("GenerateSprite: temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op after a successful rename

	fps := float64(frameCount+1) / durationS
	vf := fmt.Sprintf(
		"fps=%.6f,scale=120:68:force_original_aspect_ratio=decrease,pad=120:68:-1:-1,tile=%dx1",
		fps, frameCount,
	)
	// 不传 -noautorotate：ffmpeg 默认依据显示矩阵在滤镜前自动转正，竖屏手机
	// 视频因此是正立的，再由 scale+pad 居中补黑边塞进固定的 120×68。
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", videoPath,
		"-vf", vf,
		"-frames:v", "1",
		"-q:v", "6",
		"-y", tmpPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg GenerateSprite: %w — %s", err, string(out))
	}
	if fi, err := os.Stat(tmpPath); err != nil || fi.Size() == 0 {
		return fmt.Errorf("ffmpeg GenerateSprite: empty output")
	}
	return os.Rename(tmpPath, outPath)
}
```

- [x] **Step 4: 运行测试，确认通过**

Run: `CGO_ENABLED=1 go test ./pkg/ffmpeg/ -run TestGenerateSprite -v`
Expected: PASS（无 ffmpeg 的机器上 `TestGenerateSpriteProducesTile` SKIP，`TestGenerateSpriteRejectsZeroDuration` PASS）。

- [x] **Step 5: 提交**

```bash
git add pkg/ffmpeg/ffmpeg.go pkg/ffmpeg/ffmpeg_test.go
git commit -m "feat(ffmpeg): add GenerateSprite for hover-preview sprites"
```

---

### Task 2: SpriteGenerator（并发限流 + 去重 + 帧数工具）

**Files:**
- Create: `service/sprite.go`
- Test: `service/sprite_test.go`

**Interfaces:**
- Consumes: `ffmpeg.GenerateSprite`（Task 1）
- Produces:
  - `func NewSpriteGenerator() *SpriteGenerator`
  - `func (g *SpriteGenerator) Ensure(srcPath, outPath string, durationMs int64) (frameCount int, err error)` —
    `outPath` 已存在则立即返回；否则全局信号量（cap 2）+ per-outPath 去重后调用 `ffmpeg.GenerateSprite`。
  - `func SpriteFrameCount(durationMs int64) int` — `clamp(durationMs/30000, 5, 20)`
  - `func SpriteFrameCountFromFile(path string) (int, error)` — 读 JPEG 宽 ÷ 120
  - 常量 `SpriteFrameW = 120`、`SpriteFrameH = 68`

- [x] **Step 1: 写失败测试**

`service/sprite_test.go`：

```go
package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestSpriteFrameCountClamp(t *testing.T) {
	require.Equal(t, 5, service.SpriteFrameCount(0))        // 下限
	require.Equal(t, 5, service.SpriteFrameCount(30_000))   // 30s → 1 → 钳到 5
	require.Equal(t, 10, service.SpriteFrameCount(300_000)) // 5min → 10
	require.Equal(t, 20, service.SpriteFrameCount(600_000)) // 10min → 20
	require.Equal(t, 20, service.SpriteFrameCount(7_200_000)) // 2h → 钳到 20
}

func TestEnsureSkipsWhenExists(t *testing.T) {
	g := service.NewSpriteGenerator()
	dir := t.TempDir()
	out := filepath.Join(dir, "sprite.jpg")
	require.NoError(t, writeFakeJPEG(t, out, 10*service.SpriteFrameW, service.SpriteFrameH))
	// 文件已存在 → 不调用 ffmpeg，srcPath 给假路径也应成功
	fc, err := g.Ensure("/does/not/matter.mp4", out, 300_000)
	require.NoError(t, err)
	require.Equal(t, 10, fc)
}

func TestSpriteFrameCountFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jpg")
	require.NoError(t, writeFakeJPEG(t, p, 7*service.SpriteFrameW, service.SpriteFrameH))
	n, err := service.SpriteFrameCountFromFile(p)
	require.NoError(t, err)
	require.Equal(t, 7, n)
}
```

在 `service/sprite_test.go` 追加测试辅助（写一张指定宽高的纯色 JPEG）：

```go
import (
	"image"
	"image/jpeg"
	"os"
)

func writeFakeJPEG(t *testing.T, path string, w, h int) error {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, nil)
}
```

（把两个 import 块合并到文件顶部一个 import 区。）

- [x] **Step 2: 运行测试，确认失败**

Run: `CGO_ENABLED=1 go test ./service/ -run 'TestSpriteFrameCount|TestEnsureSkips|TestSpriteFrameCountFromFile' -v`
Expected: 编译失败 `undefined: service.SpriteFrameCount` 等。

- [x] **Step 3: 实现 service/sprite.go**

```go
package service

import (
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	"os"
	"sync"

	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
)

const (
	SpriteFrameW = 120
	SpriteFrameH = 68

	spriteMinFrames  = 5
	spriteMaxFrames  = 20
	spriteSecsPerFrm = 30
	spriteMaxConcurrent = 2
)

// SpriteFrameCount returns clamp(durationSeconds/30, 5, 20).
func SpriteFrameCount(durationMs int64) int {
	n := int(durationMs / 1000 / spriteSecsPerFrm)
	if n < spriteMinFrames {
		return spriteMinFrames
	}
	if n > spriteMaxFrames {
		return spriteMaxFrames
	}
	return n
}

// SpriteFrameCountFromFile derives the actual frame count from a sprite image's
// width (width / 120). tile=Nx1 makes width deterministically N*120.
func SpriteFrameCountFromFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, err
	}
	if cfg.Width < SpriteFrameW {
		return 0, fmt.Errorf("sprite width %d too small", cfg.Width)
	}
	return cfg.Width / SpriteFrameW, nil
}

// SpriteGenerator lazily generates hover-preview sprites with a global
// concurrency cap and per-output deduplication.
type SpriteGenerator struct {
	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func NewSpriteGenerator() *SpriteGenerator {
	return &SpriteGenerator{
		sem:      make(chan struct{}, spriteMaxConcurrent),
		inflight: make(map[string]chan struct{}),
	}
}

// Ensure makes sure outPath holds a sprite for srcPath, generating it on demand.
// durationMs must be > 0 (caller resolves it). Returns the frame count used.
func (g *SpriteGenerator) Ensure(srcPath, outPath string, durationMs int64) (int, error) {
	fc := SpriteFrameCount(durationMs)
	if _, err := os.Stat(outPath); err == nil {
		return fc, nil
	}

	// Per-output dedup: the first caller leads generation; others wait, then
	// read the just-written file.
	g.mu.Lock()
	if ch, ok := g.inflight[outPath]; ok {
		g.mu.Unlock()
		<-ch
		if _, err := os.Stat(outPath); err == nil {
			return fc, nil
		}
		return 0, errors.New("sprite generation failed (joined leader)")
	}
	ch := make(chan struct{})
	g.inflight[outPath] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.inflight, outPath)
		close(ch)
		g.mu.Unlock()
	}()

	g.sem <- struct{}{}
	defer func() { <-g.sem }()

	if err := ffmpeg.GenerateSprite(srcPath, outPath, fc, float64(durationMs)/1000.0); err != nil {
		return 0, err
	}
	return fc, nil
}
```

- [x] **Step 4: 运行测试，确认通过**

Run: `CGO_ENABLED=1 go test ./service/ -run 'TestSpriteFrameCount|TestEnsureSkips|TestSpriteFrameCountFromFile' -v`
Expected: PASS。

- [x] **Step 5: 提交**

```bash
git add service/sprite.go service/sprite_test.go
git commit -m "feat(service): SpriteGenerator with concurrency cap and dedup"
```

---

### Task 3: SearchService.UpdateDurationMs（时长回写）

**Files:**
- Modify: `service/search.go`（在 `GetAsset` 附近新增方法）
- Test: `service/search_test.go`（追加）

**Interfaces:**
- Produces: `func (s *SearchService) UpdateDurationMs(id string, ms int64) error` — `UPDATE assets SET duration_ms=? WHERE id=?`

- [x] **Step 1: 写失败测试**

在 `service/search_test.go` 追加（沿用文件已有的建库辅助；若需要新建 db 用 `sqlite.Open(filepath.Join(t.TempDir(),"t.db"))`）：

```go
func TestUpdateDurationMs(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, duration_ms, status)
		VALUES('v1','/v/v1.mp4','video/mp4',0,'indexed')`)
	require.NoError(t, err)

	s := service.NewSearchService(db, nil) // ml 可为 nil（仅用非 CLIP 方法）
	require.NoError(t, s.UpdateDurationMs("v1", 62000))

	var got int64
	require.NoError(t, db.QueryRow(`SELECT duration_ms FROM assets WHERE id='v1'`).Scan(&got))
	require.Equal(t, int64(62000), got)
}
```

- [x] **Step 2: 运行测试，确认失败**

Run: `CGO_ENABLED=1 go test ./service/ -run TestUpdateDurationMs -v`
Expected: 编译失败 `s.UpdateDurationMs undefined`。

- [x] **Step 3: 实现方法**

在 `service/search.go`（`SearchService` 有 `db` 字段，沿用）新增：

```go
// UpdateDurationMs persists a (re-probed) duration back onto the asset row,
// repairing historical rows whose duration_ms was 0/missing at ingest time.
func (s *SearchService) UpdateDurationMs(id string, ms int64) error {
	_, err := s.db.Exec(`UPDATE assets SET duration_ms=? WHERE id=?`, ms, id)
	return err
}
```

> 若 `SearchService` 的 db 字段名不是 `db`，用 `grep -n "db " service/search.go` 核实后替换。

- [x] **Step 4: 运行测试，确认通过**

Run: `CGO_ENABLED=1 go test ./service/ -run TestUpdateDurationMs -v`
Expected: PASS。

- [x] **Step 5: 提交**

```bash
git add service/search.go service/search_test.go
git commit -m "feat(service): UpdateDurationMs for sprite duration write-back"
```

---

### Task 4: Sprite handler

**Files:**
- Modify: `route/v1/assets.go`（`AssetsHandler` 加字段、`NewAssetsHandler` 构造 generator、新增 `Sprite` 方法）
- Test: `route/v1/assets_sprite_test.go`

**Interfaces:**
- Consumes: `service.SpriteGenerator`（Task 2）、`SearchService.GetAsset`/`UpdateDurationMs`（Task 3）、`ffmpeg.GetDurationMs`、`service.SpriteFrameCountFromFile`
- Produces: `func (h *AssetsHandler) Sprite(c echo.Context) error`（GET `/assets/:id/sprite`）

handler 行为：404（非视频/资产不存在/源文件丢失/时长未知）、503（ffmpeg 不可用/生成失败）、200（图片 + `X-Sprite-*` 头 + 7 天缓存）。

- [x] **Step 1: 写失败测试**

`route/v1/assets_sprite_test.go`（参考 `favorites_test.go` 的 harness 搭法；`AssetsHandler` 用真实 `service`）：

```go
package v1_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// fakeSpriteSvc embeds service.Services and overrides only Search(), the one
// method the Sprite handler uses. (Same pattern as favorites_test.go; named
// differently to avoid colliding with that file's fakeServices in package v1_test.)
type fakeSpriteSvc struct {
	service.Services
	search *service.SearchService
}

func (f *fakeSpriteSvc) Search() *service.SearchService { return f.search }

func newSpriteHarness(t *testing.T) (*v1.AssetsHandler, *sql.DB, func()) {
	t.Helper()
	thumb := t.TempDir()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	svc := &fakeSpriteSvc{search: service.NewSearchService(db, nil)}
	h := v1.NewAssetsHandler(svc, thumb)
	return h, db, func() { db.Close() }
}

func TestSpriteNotAVideo(t *testing.T) {
	h, db, cleanup := newSpriteHarness(t)
	defer cleanup()
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,status) VALUES('p1','/x/p1.jpg','image/jpeg','indexed')`)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("p1")
	err := h.Sprite(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestSpriteGeneratesAndServes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	h, db, cleanup := newSpriteHarness(t)
	defer cleanup()
	// 造源视频
	dir := t.TempDir()
	src := filepath.Join(dir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=25", "-y", src).Run())
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,mime_type,duration_ms,status)
		VALUES('v1',?, 'video/mp4', 6000, 'indexed')`, src)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("v1")
	require.NoError(t, h.Sprite(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "10", rec.Header().Get("X-Sprite-Frames"))
	require.Equal(t, "6000", rec.Header().Get("X-Sprite-Duration-Ms"))
}
```

> harness 用 `fakeSpriteSvc` 嵌入 `service.Services` 只覆写 `Search()`（与 `favorites_test.go` 同范式），无需构造完整 `services`。`NewSearchService(db, nil)` 的 `ml=nil` 对本 handler 足够（只用到 `GetAsset`/`UpdateDurationMs`）。

- [x] **Step 2: 运行测试，确认失败**

Run: `CGO_ENABLED=1 go test ./route/v1/ -run TestSprite -v`
Expected: 编译失败 `h.Sprite undefined`。

- [x] **Step 3: 改 handler 结构 + 构造函数 + Sprite 方法**

`route/v1/assets.go`：给结构体加字段、构造函数初始化、新增方法，并补 import `"strings"`, `"strconv"`(已有), `"os/exec"`, `"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"`。

```go
type AssetsHandler struct {
	svc      service.Services
	thumbDir string
	sprites  *service.SpriteGenerator
}

func NewAssetsHandler(svc service.Services, thumbDir string) *AssetsHandler {
	return &AssetsHandler{
		svc:      svc,
		thumbDir: thumbDir,
		sprites:  service.NewSpriteGenerator(),
	}
}

// Sprite serves (and lazily generates) the hover-preview sprite for a video.
func (h *AssetsHandler) Sprite(c echo.Context) error {
	asset, err := h.svc.Search().GetAsset(JWTUserID(c), c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if !strings.HasPrefix(asset.MimeType, "video/") {
		return echo.NewHTTPError(http.StatusNotFound, "not a video")
	}

	// Resolve effective duration; never feed 0 into the fps expression.
	durationMs := asset.DurationMs
	if durationMs <= 0 {
		if ms, perr := ffmpeg.GetDurationMs(asset.FilePath); perr == nil && ms > 0 {
			durationMs = ms
			_ = h.svc.Search().UpdateDurationMs(asset.ID, ms) // best-effort write-back
		}
	}
	if durationMs <= 0 {
		return echo.NewHTTPError(http.StatusNotFound, "duration unknown")
	}

	if _, serr := os.Stat(asset.FilePath); serr != nil {
		return echo.NewHTTPError(http.StatusNotFound, "source missing")
	}

	spritePath := filepath.Join(h.thumbDir, asset.ID, "sprite.jpg")
	if _, gerr := h.sprites.Ensure(asset.FilePath, spritePath, durationMs); gerr != nil {
		if errors.Is(gerr, exec.ErrNotFound) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "ffmpeg unavailable")
		}
		return echo.NewHTTPError(http.StatusServiceUnavailable, gerr.Error())
	}

	frames, ferr := service.SpriteFrameCountFromFile(spritePath)
	if ferr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, ferr.Error())
	}

	hdr := c.Response().Header()
	hdr.Set("X-Sprite-Frames", strconv.Itoa(frames))
	hdr.Set("X-Sprite-Frame-W", strconv.Itoa(service.SpriteFrameW))
	hdr.Set("X-Sprite-Frame-H", strconv.Itoa(service.SpriteFrameH))
	hdr.Set("X-Sprite-Duration-Ms", strconv.FormatInt(durationMs, 10))
	hdr.Set("Cache-Control", "max-age=604800")
	return c.File(spritePath)
}
```

- [x] **Step 4: 运行测试，确认通过**

Run: `CGO_ENABLED=1 go test ./route/v1/ -run TestSprite -v`
Expected: PASS（无 ffmpeg 时 `TestSpriteGeneratesAndServes` SKIP，`TestSpriteNotAVideo` PASS）。

- [x] **Step 5: 提交**

```bash
git add route/v1/assets.go route/v1/assets_sprite_test.go
git commit -m "feat(api): GET /assets/:id/sprite lazy hover-preview endpoint"
```

---

### Task 5: 路由注册 + JWT Skipper

**Files:**
- Modify: `route/router.go`

**Interfaces:**
- Consumes: `assets.Sprite`（Task 4）

- [x] **Step 1: 注册路由**

在 `route/router.go` 现有 asset 路由块（`g.GET("/assets/:id/live", assets.Live)` 之后）加入：

```go
	g.GET("/assets/:id/sprite", assets.Sprite)
```

- [x] **Step 2: 加入 Skipper（免 JWT，<img> 才能直连）**

在 Skipper 的后缀判断里，给 `/live` 那组追加 `/sprite`：

```go
			if strings.HasSuffix(p, "/thumbnail") ||
				strings.HasSuffix(p, "/face-thumbnail") ||
				strings.HasSuffix(p, "/original") ||
				strings.HasSuffix(p, "/live") ||
				strings.HasSuffix(p, "/sprite") ||
				strings.HasSuffix(p, "/favorites/export") {
				return true
			}
```

- [x] **Step 3: 编译 + 全量后端测试**

Run: `CGO_ENABLED=1 go build ./... && CGO_ENABLED=1 go test ./...`
Expected: 编译通过，测试全绿（无 ffmpeg 的 CI 上相关用例 SKIP）。

- [x] **Step 4: 提交**

```bash
git add route/router.go
git commit -m "feat(api): register /assets/:id/sprite and exempt it from JWT"
```

---

### Task 6: 前端 VideoHoverPreview.vue 组件

**Files:**
- Create: `src/views/Photos/VideoHoverPreview.vue`（仓库：NimoOS-UI）
- Test: `tests/videoHoverPreview.test.js`

**Interfaces:**
- Produces: 无状态展示组件。Props：`visible:Boolean, x:Number, y:Number, spriteUrl:String, frameCount:Number, frameW:Number(=120), frameH:Number(=68), currentFrame:Number, durationMs:Number`。
- 暴露方法 `loadSprite(url)`：设置内部 `spriteUrl` 并在 `<img>` `onload` 时通过 `$emit('loaded', {frameCount, durationMs})` 回传响应头解析出的元数据。

- [x] **Step 1: 写失败测试**

`tests/videoHoverPreview.test.js`（vitest + @vue/test-utils，沿用 `tests/` 现有风格）：

```js
import { mount } from '@vue/test-utils'
import VideoHoverPreview from '@/views/Photos/VideoHoverPreview.vue'

describe('VideoHoverPreview', () => {
  const base = {
    visible: true, x: 0, y: 0, spriteUrl: '/s.jpg',
    frameCount: 10, frameW: 120, frameH: 68, currentFrame: 0, durationMs: 60000,
  }

  it('positions the sprite window by currentFrame', () => {
    const w = mount(VideoHoverPreview, { propsData: { ...base, currentFrame: 3 } })
    const win = w.find('[data-test="sprite-window"]')
    expect(win.element.style.backgroundPositionX).toBe('-360px') // -(3*120)
    expect(win.element.style.backgroundSize).toBe('1200px 68px') // 10*120 x 68
  })

  it('renders the current time label from currentFrame', () => {
    const w = mount(VideoHoverPreview, { propsData: { ...base, currentFrame: 5 } })
    // 5/10 * 60000ms = 30s → "0:30"
    expect(w.find('[data-test="time-label"]').text()).toContain('0:30')
  })

  it('is hidden when visible=false', () => {
    const w = mount(VideoHoverPreview, { propsData: { ...base, visible: false } })
    expect(w.find('[data-test="sprite-window"]').exists()).toBe(false)
  })
})
```

- [x] **Step 2: 运行测试，确认失败**

Run: `yarn test tests/videoHoverPreview.test.js`
Expected: FAIL（找不到组件）。

- [x] **Step 3: 实现组件**

`src/views/Photos/VideoHoverPreview.vue`：

```vue
<template>
  <div
    v-if="visible"
    class="video-hover-preview"
    :style="{ left: x + 'px', top: y + 'px' }"
  >
    <div
      data-test="sprite-window"
      class="sprite-window"
      :style="winStyle"
    />
    <div class="bar"><div class="bar-fill" :style="{ width: progressPct + '%' }" /></div>
    <div data-test="time-label" class="time">{{ currentLabel }} / {{ totalLabel }}</div>
  </div>
</template>

<script>
function fmt(ms) {
  const s = Math.max(0, Math.floor(ms / 1000))
  const m = Math.floor(s / 60)
  return `${m}:${String(s % 60).padStart(2, '0')}`
}
export default {
  name: 'VideoHoverPreview',
  props: {
    visible: Boolean,
    x: { type: Number, default: 0 },
    y: { type: Number, default: 0 },
    spriteUrl: { type: String, default: '' },
    frameCount: { type: Number, default: 1 },
    frameW: { type: Number, default: 120 },
    frameH: { type: Number, default: 68 },
    currentFrame: { type: Number, default: 0 },
    durationMs: { type: Number, default: 0 },
  },
  computed: {
    winStyle() {
      return {
        width: this.frameW + 'px',
        height: this.frameH + 'px',
        backgroundImage: `url(${this.spriteUrl})`,
        backgroundPositionX: `${-(this.currentFrame * this.frameW)}px`,
        backgroundSize: `${this.frameCount * this.frameW}px ${this.frameH}px`,
        backgroundRepeat: 'no-repeat',
      }
    },
    progressPct() {
      return this.frameCount > 1 ? (this.currentFrame / (this.frameCount - 1)) * 100 : 0
    },
    currentLabel() {
      const p = this.frameCount > 0 ? this.currentFrame / this.frameCount : 0
      return fmt(p * this.durationMs)
    },
    totalLabel() {
      return fmt(this.durationMs)
    },
  },
}
</script>

<style scoped>
.video-hover-preview {
  position: fixed;
  z-index: 50;
  background: #000;
  border-radius: 4px;
  padding: 2px;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.4);
  pointer-events: none;
}
.sprite-window { display: block; }
.bar { height: 2px; background: rgba(255, 255, 255, 0.3); margin-top: 2px; }
.bar-fill { height: 2px; background: #fff; }
.time { color: #fff; font-size: 10px; text-align: center; margin-top: 1px; }
</style>
```

- [x] **Step 4: 运行测试，确认通过**

Run: `yarn test tests/videoHoverPreview.test.js`
Expected: PASS。

- [x] **Step 5: 提交**

```bash
git add src/views/Photos/VideoHoverPreview.vue tests/videoHoverPreview.test.js
git commit -m "feat(photos-ui): VideoHoverPreview sprite scrub component"
```

---

### Task 7: PhotosGrid.vue 接入悬停事件

**Files:**
- Modify: `src/views/Photos/PhotosGrid.vue`
- Test: `tests/photosGridHover.test.js`

**Interfaces:**
- Consumes: `VideoHoverPreview`（Task 6）；后端 `/v1/photos/assets/:id/sprite`（Task 4–5），从响应头读 `X-Sprite-Frames`/`X-Sprite-Duration-Ms`。

- [x] **Step 1: 写失败测试**

`tests/photosGridHover.test.js` —— 测纯逻辑（鼠标横向比例 → currentFrame 映射），避免依赖完整网格渲染：

```js
import { computeFrameFromX } from '@/views/Photos/hoverScrub.js'

describe('hover scrub mapping', () => {
  it('maps pointer x fraction to a frame index', () => {
    // rectLeft=100, width=200, frameCount=10
    expect(computeFrameFromX(100, 100, 200, 10)).toBe(0)   // 最左
    expect(computeFrameFromX(300, 100, 200, 10)).toBe(9)   // 最右钳到 N-1
    expect(computeFrameFromX(200, 100, 200, 10)).toBe(5)   // 中点
  })
})
```

- [x] **Step 2: 运行测试，确认失败**

Run: `yarn test tests/photosGridHover.test.js`
Expected: FAIL（`hoverScrub.js` 不存在）。

- [x] **Step 3a: 抽出可测纯函数**

`src/views/Photos/hoverScrub.js`：

```js
// 把指针的横向位置映射到帧序号 [0, frameCount-1]
export function computeFrameFromX(clientX, rectLeft, rectWidth, frameCount) {
  if (rectWidth <= 0 || frameCount <= 0) return 0
  const p = (clientX - rectLeft) / rectWidth
  const idx = Math.floor(p * frameCount)
  if (idx < 0) return 0
  if (idx > frameCount - 1) return frameCount - 1
  return idx
}
```

- [x] **Step 3b: 接入 PhotosGrid.vue**

在 `<template>` 视频 tile 上加事件（仅 `p.isVideo` 时），并在根挂载 `<VideoHoverPreview>`：

```html
<div
  class="tile"
  @click="onTileClick(p)"
  @mouseenter="p.isVideo && onVideoEnter(p, $event)"
  @mousemove="p.isVideo && onVideoMove($event)"
  @mouseleave="p.isVideo && onVideoLeave()"
>
```

```html
<VideoHoverPreview
  ref="preview"
  :visible="previewVisible"
  :x="previewPos.x" :y="previewPos.y"
  :sprite-url="spriteUrl"
  :frame-count="spriteFrameCount"
  :frame-w="120" :frame-h="68"
  :current-frame="currentFrame"
  :duration-ms="spriteDurationMs"
/>
```

`script` 引入组件与纯函数，新增 data 与方法：

```js
import VideoHoverPreview from './VideoHoverPreview.vue'
import { computeFrameFromX } from './hoverScrub.js'

export default {
  components: { VideoHoverPreview },
  data() {
    return {
      hoveredVideo: null,
      previewVisible: false,
      previewPos: { x: 0, y: 0 },
      currentFrame: 0,
      spriteUrl: '',
      spriteFrameCount: 10,
      spriteDurationMs: 0,
      _hoverTimer: null,
      _tileRect: null,
    }
  },
  methods: {
    onVideoEnter(p, e) {
      clearTimeout(this._hoverTimer)
      const target = e.currentTarget
      this._hoverTimer = setTimeout(async () => {
        this._tileRect = target.getBoundingClientRect()
        this.hoveredVideo = p
        this.previewPos = { x: this._tileRect.left, y: this._tileRect.top - 68 - 8 }
        this.currentFrame = 0
        this.spriteDurationMs = p.durationMs || 0
        this.previewVisible = true
        const url = `/v1/photos/assets/${p.id}/sprite`
        try {
          const resp = await fetch(url)
          if (!resp.ok) { this.previewVisible = false; return } // 404/503 → 保持静态缩略图
          this.spriteFrameCount = parseInt(resp.headers.get('X-Sprite-Frames') || '10', 10)
          const d = parseInt(resp.headers.get('X-Sprite-Duration-Ms') || '0', 10)
          if (d > 0) this.spriteDurationMs = d
          this.spriteUrl = url // 此时已生成并缓存，<img> 命中
        } catch (_) {
          this.previewVisible = false
        }
      }, 300)
    },
    onVideoMove(e) {
      if (!this._tileRect || !this.hoveredVideo) return
      this.currentFrame = computeFrameFromX(
        e.clientX, this._tileRect.left, this._tileRect.width, this.spriteFrameCount,
      )
    },
    onVideoLeave() {
      clearTimeout(this._hoverTimer)
      this.previewVisible = false
      this.hoveredVideo = null
    },
  },
}
```

> 上面只新增成员；保留 PhotosGrid 现有 data/methods/components，把这些键合并进去，不要整体覆盖。

- [x] **Step 4: 运行测试 + lint**

Run: `yarn test tests/photosGridHover.test.js && yarn lint`
Expected: PASS / 无 lint 错误。

- [x] **Step 5: 提交**

```bash
git add src/views/Photos/PhotosGrid.vue src/views/Photos/hoverScrub.js tests/photosGridHover.test.js
git commit -m "feat(photos-ui): hover-to-scrub video preview in PhotosGrid"
```

---

### Task 8: 端到端手测 + 收尾

**Files:** 无（验收）

- [ ] **Step 1: 起后端**

Run: `cd NimoOS-Photos && CGO_ENABLED=1 go build -o nimoos-photos . && ./nimoos-photos`（或按现有 dev 流程）。

- [ ] **Step 2: 起前端 dev**

Run: `cd NimoOS-UI && yarn dev`

- [ ] **Step 3: 手动验收清单**

- 悬停一个**新**视频 tile：~300ms 后出现预览；首次稍慢（同步生成），之后秒开。
- 鼠标在 tile 上左右移动：帧随位置变化，时间标签随之更新。
- 悬停一个**图片** tile：无预览（后端 404）。
- 直接二次悬停同一视频：立即命中缓存（`<thumbDir>/<id>/sprite.jpg` 已存在）。
- 删除该视频后，确认 `<thumbDir>/<id>/` 整目录连带 `sprite.jpg` 消失（无需额外代码）。

- [x] **Step 4: 全量测试回归**

Run: `cd NimoOS-Photos && CGO_ENABLED=1 go test ./...` 与 `cd NimoOS-UI && yarn test`
Expected: 全绿。

---

## 范围外（后续单独立项）

- **FilePanel.vue 复用** `VideoHoverPreview`：组件已无状态可直接复用，但属于另一视图，单独一个小 PR 跟进（spec §5.3）。
- **Layer 2 视频语义检索**：走「视频切块向量化」独立路线，与本 sprite 流水线无关（spec 非目标）。
