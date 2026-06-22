# 视频悬停预览设计

**日期**: 2026-06-18（2026-06-22 重写收窄；2026-06-22 前端改为 tile 内覆盖层）
**涉及服务**: NimoOS-Photos（后端）、NimoOS-UI（前端）
**状态**: 后端已实现；前端首版（tile 上方浮窗）已实现，现改为 tile 内覆盖层（YouTube 式就地预览）

---

## 一、背景与目标

NimoOS-Photos 的图库中，视频资产目前只展示静态缩略图 + 时长角标，用户无法在不打开视频的情况下快速了解内容。

**目标**：
- 鼠标悬停视频 tile 时自动预览视频内容
- 鼠标在 tile 上左右移动（无需点击）即可 scrub 到不同时间段

**非目标（明确排除）**：
- **视频语义检索 / 向量化不在本设计范围内**。将来的视频检索会走独立的「视频切块向量化」路线，与本设计的 sprite 抽帧流水线无关。本文档因此不预留任何检索用的帧文件、向量表或 224px 帧规格——sprite 是纯展示缓存，单一职责。

---

## 二、整体架构（纯懒加载）

预览图采用 **sprite 雪碧图**：把视频均匀采样出的 N 帧横向拼成一张 JPEG，前端通过 `background-position-x` 在单帧窗口里切换显示。

**核心策略：纯懒加载，按需生成。**

```
入库流水线（processFile）
   └─ 不碰 sprite，零改动，视频入库速度不受影响

用户首次悬停视频 tile
   └─→ GET /assets/:id/sprite
            ├─ sprite.jpg 已存在 → 直接返回 200
            └─ 不存在 → 在该 HTTP 请求的 goroutine 上同步用 ffmpeg 生成
                       → 落盘永久缓存 → 返回 200
```

设计要点：
- **入库不预生成**：视频可能上万，但用户实际悬停的只是极小一部分。只为「被看过」的视频付生成代价。
- **不开后台线程**：生成跑在请求自身的 goroutine 上，没有独立 worker 池 / 队列 / 插队逻辑。
- **新老视频统一**：无论是功能上线前已入库的存量视频，还是新入库视频，都走同一条「serve 时按需生成」路径，无需存量补齐逻辑。
- **永久缓存**：生成一次后落盘，后续请求直接命中。

### 并发控制

虽然是同步生成，仍设一个**进程级全局信号量 = 2**：用户在网格里快速划过多个视频时，300ms 防抖后仍可能并发触发多个 ffmpeg，信号量把同时运行的 ffmpeg 限制在 2 个，保护 NAS 的 CPU/IO。超出的请求排队等待（仍返回 200，只是稍慢）。

此外用 per-assetID 的 singleflight（`sync.Map` + 等待）合并对**同一视频**的并发请求，避免重复生成同一张 sprite。

---

## 三、抽帧与帧规格

### 3.1 抽帧策略

**均匀采样，自适应帧数，上下限 5–20：**

```
frameCount = clamp(durationSeconds / 30, 5, 20)
```

| 视频时长 | 帧数 | 帧间隔 |
|---|---|---|
| ≤ 2.5 分钟 | 5（下限） | ≤ 30 秒 |
| 5 分钟 | 10 | 30 秒 |
| 10 分钟 | 20（上限） | 30 秒 |
| 2 小时 | 20（上限） | 6 分钟 |

上限 20 的依据：tile 宽约 200px，超过 20 帧后每帧覆盖 < 10px，落在鼠标抖动误差内，用户感知不到差异。

`durationSeconds` 来自有效时长 `D`（优先 `assets.duration_ms`，为 0 时 ffprobe 重探，见 4 节）。`D` 必须 > 0，否则 `fps` 表达式非法。

### 3.2 帧规格

- 分辨率：**固定 120×68 px**（16:9）
- **autorotate**：依据视频旋转元数据自动转正，保证竖屏（手机 9:16）视频的帧是正立的；竖屏帧居中补黑边塞进 120×68。固定帧尺寸让 sprite 每帧等宽、前端定位数学最简单，黑边在 120px 的小预览里不刺眼。
- 格式：JPEG，**`-q:v 6`**（ffmpeg mjpeg 刻度 2–31，越小越好；小图够清晰，体积仍小）
- 拼合：横向 tile，`frameCount × 1`
- 产物：`sprite.jpg`，单文件，目标 ≤ 20KB

> **画质说明**：sprite 是刻意重压的有损缩略图，且只读取原视频的帧另存为独立 JPEG——**绝不重新编码、绝不修改原视频**，也不影响现有的 `small.jpg`/`large.jpg`。

### 3.3 ffmpeg 命令

```bash
ffmpeg -hide_banner -i <videoPath> \
  -vf "fps=(<frameCount>+1)/<durationS>,\
       scale=120:68:force_original_aspect_ratio=decrease,\
       pad=120:68:-1:-1,\
       tile=<frameCount>x1" \
  -frames:v 1 -q:v 6 -y <thumbDir>/<assetID>/sprite.jpg
```

关键机制：

- **`tile=Nx1` + `-frames:v 1`**：`tile` 把每 N 个输入帧合成 1 张横向拼图，`-frames:v 1` 只取第一张完整拼图，多余的输入帧（见下）被自然丢弃。输出宽恒为 N×120，因此 `frameCount = 宽 ÷ 120` 是**确定性**的（见 3.4），缓存命中时读宽即得，无需额外存储。
- **`fps=(N+1)/D` 过采样 1 帧**：纯属防御。规整的 CFR 视频用 `fps=N/D` 即可稳定产出 N 帧；但病态 VFR / 手机视频理论上可能在 EOF 丢掉最后一帧，使 `tile` 末格被填成黑色。过采样保证喂进 `tile` 的帧 ≥ N，末格永不为黑；代价只是采样点整体微微前移（末帧落在 ≈ (N-1)D/(N+1) 处），scrub 预览无感。

> **不要用 `-vframes` 去"截断"帧数**：`-vframes`（= `-frames:v`）是**输出选项**，不能写进 `-vf` 滤镜图（会直接报 `No option name` 解析错误），且它限制的是输出帧数——`tile` 之后输出本就只有 1 帧，与"喂进 tile 的输入帧数"无关。控制满格只能靠过采样，不能靠 `-vframes`。

### 3.4 帧 ↔ 时间映射（不存时间戳，靠算）

均匀采样使帧与时间是纯线性关系，**无需为每帧存时间戳**。前端只需两个数：

1. **`durationMs`**：以 sprite 响应头 `X-Sprite-Duration-Ms`（生成时确定的有效时长）为准，回退到 `asset.durationMs`。
2. **`frameCount`**：sprite **实际**帧数 = sprite 图片宽度 ÷ 120，服务端在响应时读图算出，通过 `X-Sprite-Frames` 头返回（不进数据库）。

前端映射（`p` = 鼠标在 tile 上的横向比例，∈ [0,1)）：
- 显示第几帧：`frame = floor(p × frameCount)`
- 时间标签：`当前时刻 = p × durationMs`
- （反向）第 `i` 帧大致对应视频 `(i + 0.5) / frameCount × duration` 处

### 3.5 存储路径

```
<thumbDir>/<assetID>/
  small.jpg       ← 已有
  large.jpg       ← 已有
  sprite.jpg      ← 新增
```

`<thumbDir>` = `<DataPath>/thumbs`，实际为 `/DATA/.system_data/photos/thumbs/`（`DataPath` 配置在 `/etc/nimoos/photos.conf`）。sprite.jpg 是纯缓存，可随时从原视频重建。

### 3.6 删除与清理

- **删除资产**：零新增代码。永久删除（`trash.go:175` `PurgeAsset`）与重新索引/移除（`indexer.go:1158`、`1196`）均执行 `os.RemoveAll(filepath.Join(thumbDir, id))`，整目录删除，连带删掉 sprite.jpg。
- **prune**：`/cache/prune`（孤儿清理）需把 `sprite.jpg` 纳入识别，避免误删有效 sprite；sprite 可重建，清理无损。

---

## 四、后端接口

### `GET /assets/:id/sprite`

行为：
1. 查 `SELECT file_path, mime_type, duration_ms FROM assets WHERE id=? AND status='indexed'`。
2. 资产不存在 / 非视频（`mime_type` 不以 `video/` 开头）→ **404**。
3. `sprite.jpg` 已存在 → 读宽度算 frameCount，返回 200 + 图片。
4. 不存在 → **解析有效时长 `D`（见下）** → 取全局信号量（限 2）+ per-asset singleflight → ffmpeg 同步生成 → 返回 200。
5. `file_path` 在磁盘上不存在 → **404**。
6. ffmpeg 不在 PATH / 生成失败 → **503**。

**有效时长解析（必须在生成前完成，绝不让 0 进入 fps 表达式）：**

`fps=(N+1)/D` 在 `D=0`（或空）时 ffmpeg 直接报 `Error reinitializing filters! Invalid argument (-22)` 且不产出文件。因此：

1. `D = assets.duration_ms`；
2. 若 `D <= 0` → 当场 `ffprobe -show_entries format=duration` 重新探测（生成本就要起 ffmpeg，多一次 probe 开销可忽略），探到则回写 `assets.duration_ms` 修正历史数据；
3. 若仍 `D <= 0`（极少数损坏/分片流，duration 与 nb_frames 同时缺失）→ **返回 404，不生成 sprite**，前端保持静态 `small.jpg`。该（病态）视频无悬停预览。

`X-Sprite-Duration-Ms` 返回的是**这个有效 `D`**（可能是重探得到的值），前端时间标签以该响应头为准，优先于 `asset.durationMs`。

源文件路径解析复用现有 `assets.Original`（`route/v1/assets.go:173`）的 `file_path` 取法。

| 项目 | 说明 |
|---|---|
| JWT | 豁免（与 `/thumbnail`、`/original` 一致，加入 `router.go` Skipper） |
| 缓存 | `Cache-Control: max-age=604800`（7 天） |

**响应头（元数据）：**
```
X-Sprite-Frames:      10          // 实际帧数 = 图片宽度 / 120
X-Sprite-Frame-W:     120
X-Sprite-Frame-H:     68
X-Sprite-Duration-Ms: 62000       // = assets.duration_ms
```

前端从响应头读取元数据，无需额外接口。

### 路由注册（`route/router.go`）

```go
g.GET("/assets/:id/sprite", assets.Sprite)
```

Skipper 增加：
```go
strings.HasSuffix(p, "/sprite")
```

### Live Photo 视频

`is_live_photo_video = 1` 的资产是实况照片的视频半身，`timeline.List` 不把它们当独立 tile 显示，因此 `/sprite` 不会被请求到它们——无需特殊处理。

---

## 五、前端实现

> **2026-06-22 调整（采纳方案 A）**：预览不再是悬浮在 tile **上方**的独立小窗，而是**就地覆盖在视频 tile 内部**（YouTube 网格预览式）。当前帧以 `cover` 方式填满整个 tile（与静态缩略图同样的裁切），底部叠一条细进度条；不再显示大的时间文字。下文反映改版后的设计。

### 5.1 组件 `VideoHoverPreview.vue`（tile 内覆盖层）

路径：`src/views/Photos/VideoHoverPreview.vue`，无状态纯展示组件。**填满父容器**（`position: absolute; inset: 0`），由宿主（视频 tile，`position: relative`）决定尺寸，组件自身不关心绝对坐标。

**Props：**

| Prop | 类型 | 说明 |
|---|---|---|
| `visible` | Boolean | 是否显示（父组件仅在悬停该 tile 时为 true） |
| `spriteUrl` | String | `/v1/photos/assets/:id/sprite` |
| `frameCount` | Number | 来自响应头 X-Sprite-Frames |
| `frameW` | Number | 120 |
| `frameH` | Number | 68 |
| `currentFrame` | Number | 0 ~ frameCount-1，由父组件根据鼠标横向位置驱动 |

> 去掉 `x` / `y`（不再 fixed 定位）与 `durationMs`（不再显示时间文字）。

**UI 结构：**

```
┌──────────────────────────┐
│                          │
│   [当前帧 · cover 填满]   │  ← 覆盖整个 tile
│   background-position-x  │    选择第 currentFrame 帧
│                          │
│ ████████░░░░░░░░░░░░░░░░ │  ← 底部细进度条（随鼠标位置）
└──────────────────────────┘
```

**帧定位 CSS（cover 填满任意尺寸 tile，N 帧横向 sprite）：**

设 tile 可见区为 `W×H`，单帧原始 `120×68`。为让单帧 cover 填满 `W×H`，整张 sprite 等比缩放因子 `s = max(W/120, H/68)`：

```js
// 覆盖层填满 tile
position: 'absolute', inset: 0,
backgroundImage: `url(${spriteUrl})`,
backgroundRepeat: 'no-repeat',
// 整张 sprite 缩放后：宽 = frameCount*120*s，高 = 68*s
backgroundSize: `${frameCount * frameW * s}px ${frameH * s}px`,
// 横向定位到第 currentFrame 帧（再按需要做竖直居中）
backgroundPositionX: `${-(currentFrame * frameW * s)}px`,
backgroundPositionY: 'center',
```

> `s` 在组件内用 `ResizeObserver` 或挂载时读自身 `clientWidth/clientHeight` 计算；tile 尺寸随密度（density）变化，故按实际盒子算，避免写死。底部进度条为一个绝对定位在底边的 `div`，宽度 = `currentFrame/(frameCount-1)*100%`。

**加载与 fallback**：sprite 加载完成前显示原静态 `small.jpg`（即 tile 原本的 `<img>`，覆盖层透明/未显示）；加载完成后覆盖层显示、无缝盖住缩略图。`/sprite` 返回 503/404 时覆盖层不显示，tile 保持静态缩略图，不弹错。原时长角标（`.tile-vid`）始终保留。

### 5.2 PhotosGrid.vue 改动

在视频 tile（`p.isVideo === true`，`position: relative`）上增加三个鼠标事件：`mouseenter` / `mousemove` / `mouseleave`；并在视频 tile **内部**挂载 `<VideoHoverPreview>`，仅当 `hoveredVideo === p` 且 sprite 已就绪时渲染：

```html
<div class="tile" ... @mouseenter="p.isVideo && onVideoEnter(p,$event)"
     @mousemove="p.isVideo && onVideoMove($event)"
     @mouseleave="p.isVideo && onVideoLeave()">
  <img :src="thumbnailSrc(p.id)" .../>      <!-- 静态缩略图，始终在底层 -->
  ...
  <VideoHoverPreview
    v-if="p.isVideo && hoveredVideo === p"
    :visible="previewVisible"
    :sprite-url="spriteUrl"
    :frame-count="spriteFrameCount"
    :frame-w="120" :frame-h="68"
    :current-frame="currentFrame"
  />
  <div v-if="p.isVideo" class="tile-vid">…时长角标…</div>  <!-- 保留，盖在最上 -->
</div>
```

**data 新增：**（去掉 `previewPos`；定位由 tile 内 `inset:0` 决定）
```js
hoveredVideo: null,
previewVisible: false,
currentFrame: 0,
spriteUrl: '',
spriteFrameCount: 10,
spriteDurationMs: 0,   // 仍可用于时长角标，不再传给预览组件
hoverTimer: null,      // 不用 _ 前缀（Vue data 保留 _ 开头键，lint vue/no-reserved-keys）
tileRect: null,
```

**交互方法：**

```js
onVideoEnter(p, e) {
  clearTimeout(this.hoverTimer)
  const target = e.currentTarget
  this.hoverTimer = setTimeout(async () => {
    this.tileRect = target.getBoundingClientRect()  // 仅用于鼠标 X→帧 的比例换算
    this.hoveredVideo = p
    this.currentFrame = 0
    this.previewVisible = true
    const url = `/v1/photos/assets/${p.id}/sprite`
    try {
      const resp = await fetch(url)
      if (!resp.ok) { this.previewVisible = false; this.hoveredVideo = null; return } // 404/503 → 保持静态缩略图
      this.spriteFrameCount = parseInt(resp.headers.get('X-Sprite-Frames') || '10', 10)
      this.spriteUrl = url   // 已生成并缓存，覆盖层背景命中
    } catch (_) {
      this.previewVisible = false; this.hoveredVideo = null
    }
  }, 300)  // 300ms debounce，防止快速扫过
},

onVideoMove(e) {
  if (!this.tileRect || !this.hoveredVideo) return
  this.currentFrame = computeFrameFromX(
    e.clientX, this.tileRect.left, this.tileRect.width, this.spriteFrameCount,
  )
},

onVideoLeave() {
  clearTimeout(this.hoverTimer)
  this.previewVisible = false
  this.hoveredVideo = null
  this.spriteUrl = ''
},
```

`computeFrameFromX`（纯函数，`src/views/Photos/hoverScrub.js`，已实现且不变）把鼠标横向比例映射到 `[0, frameCount-1]`。300ms 防抖确保只有「停下来看」才触发请求，避免快速划过网格时为每个视频都打 `/sprite`。

### 5.3 复用到文件浏览器

`VideoHoverPreview.vue` 是无状态纯展示组件、**填满父容器**（`inset:0`），`FilePanel.vue` 可直接引入：
- 把组件挂进文件项的视频缩略图容器内（容器需 `position: relative`）
- 同样的三个鼠标事件 + `computeFrameFromX`
- 接口统一走 `/v1/photos/assets/:id/sprite`
- 仅限已入库（有 assetID）的视频

---

## 六、边界情况

| 场景 | 处理 |
|---|---|
| 视频时长 < 10 秒 | frameCount = 5（下限） |
| `duration_ms` 缺失或为 0 | 先 ffprobe 重探时长并回写；探到则正常生成（`D=0` 会让 `fps` 表达式非法、ffmpeg 报 -22 不产出文件）；仍探不到 → 404，前端 fallback |
| 竖屏 / 非 16:9 视频 | autorotate 转正 + pad 补黑边，固定 120×68 |
| 同一视频并发请求 | per-assetID singleflight，合并为一次生成 |
| 多视频并发请求 | 全局信号量限 2 个 ffmpeg，其余排队 |
| ffmpeg 不在 PATH | `/sprite` 返回 503，前端 fallback 静态 thumbnail |
| 原文件已删除/移动 | `/sprite` 返回 404，前端 fallback |
| 已删除视频 | 整目录 RemoveAll 已连带删除 sprite.jpg |

---

## 七、存储估算

| 场景 | 每视频 | 1万视频（全部被悬停过的极端上限） |
|---|---|---|
| 短视频（< 5 分钟） | ~8 KB | ~80 MB |
| 长视频（> 10 分钟，上限 20 帧） | ~20 KB | ~200 MB |
| 混合估算 | ~12 KB | ~120 MB |

实际占用远低于上限：纯懒加载只为被悬停过的视频生成 sprite。

---

## 八、实现顺序

```
1. pkg/ffmpeg: 新增 GenerateSprite(videoPath, outPath, frameCount)
2. service: sprite 生成入口（全局信号量 2 + per-asset singleflight + 文件存在性判断）
3. route/v1/assets.go: 新增 Sprite handler（404/503/200 + X-Sprite-* 头）
4. route/router.go: 注册路由 + JWT Skipper 增加 /sprite
5. service: /cache/prune 识别 sprite.jpg
6. NimoOS-UI: VideoHoverPreview.vue 组件（含 small.jpg fallback）
7. NimoOS-UI: PhotosGrid.vue 接入悬停事件
8. NimoOS-UI: FilePanel.vue 复用组件（同接口）
```
