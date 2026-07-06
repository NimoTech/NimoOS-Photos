# NimoOS-Photos

NimoOS 的相册服务，提供**照片/视频索引、EXIF 解析、缩略图生成、人脸识别、地理反编码、CLIP 语义搜索**。当前版本 `v1.9.0-alpha1`。

绑定 localhost 随机端口、由 Gateway 转发，API 前缀 `/v1/photos`；TUS 断点续传上传前缀 `/v1/upload-tus`。

---

## 整体架构

```
外部请求（Gateway 转发，/v1/photos/* 和 /v1/upload-tus/*）
                        │
                        ▼
        ┌───────────────────────────────────────┐
        │   nimoos-photos.service (Go, Echo)    │
        │   - JWT 鉴权（含豁免规则）             │
        │   - 资产 CRUD / 时间线 / 相册          │
        │   - 收藏 / 回收站 / 人物 / 地点        │
        │   - Smart View 语义自动相册           │
        │   - TUS 断点续传上传（/DATA/Gallery） │
        └───────────────────────────────────────┘
               │           │            │
    ┌──────────▼──┐  ┌──────▼────────┐  │
    │  Watcher    │  │   Indexer     │  │
    │ (fsnotify)  │→─│ (3 workers)   │  │
    │ /DATA/Gallery│  │ SHA-256去重   │  │
    │ /DATA/...   │  └──────┬────────┘  │
    └─────────────┘         │           │
                     ┌──────▼───────────▼──────────────┐
                     │   SQLite (WAL) + sqlite-vec       │
                     │  assets / asset_exif             │
                     │  face_detections / persons        │
                     │  clip_embeddings (vec0, 1152-dim) │
                     │  asset_ocr / asset_geo            │
                     │  albums / smart_views / ...       │
                     └───────────────────────────────────┘
                                    │
                     ┌──────────────▼──────────────────────────┐
                     │  immich-machine-learning (Docker)       │
                     │  127.0.0.1:3003  /predict               │
                     │  - CLIP nllb-clip-large-siglip__v1(1152维)│
                     │  - 人脸检测+识别 antelopev2 (512-dim)     │
                     │  - OCR PP-OCRv5_server                  │
                     └───────────────────────────────────────┘
```

ML 后端为独立 Docker Compose 栈（`deploy/ml/docker-compose.yml`），绑定 `127.0.0.1:3003`，镜像离线捆绑、不联网拉取。

---

## API 路由（`/v1/photos`）

| Method | Path | 用途 |
|---|---|---|
| GET | `/assets` | 列出资产（支持分页、地点/位置过滤） |
| POST | `/assets/upload` | 普通上传（小文件） |
| GET | `/assets/:id` | 获取单个资产 |
| DELETE | `/assets/:id` | 删除资产 |
| GET | `/assets/:id/thumbnail` | 获取缩略图（JWT 豁免） |
| GET | `/assets/:id/original` | 获取原始文件（JWT 豁免） |
| GET | `/assets/:id/live` | 获取 Live Photo 视频（JWT 豁免） |
| GET | `/timeline` | 按年月分组的时间线 |
| POST | `/search/smart` | CLIP 语义搜索 + OCR 精确匹配（localhost 免 JWT） |
| GET | `/search/faces/:person_id` | 按人物查找资产 |
| GET/POST/DELETE | `/albums[/:id]` | 相册 CRUD |
| POST/DELETE | `/albums/:id/assets[/batch]` | 相册资产添加/删除 |
| PATCH | `/albums/:id` | 相册元数据更新 |
| PATCH | `/albums/:id/assets/order` | 手动排序 |
| GET | `/places` | 地点列表（按城市聚合） |
| GET | `/places/:key` | 指定地点详情 |
| GET | `/places/:key/cover-candidates` | 封面候选 |
| PUT/DELETE | `/places/:key/cover` | 设置/重置地点封面 |
| PUT/DELETE | `/places/:key/spot-name` | 设置/重置打点名 |
| POST | `/places/:key/album` | 以地点创建相册 |
| POST/DELETE | `/favorites/:asset_id` | 收藏/取消收藏 |
| GET | `/favorites[/ids/export/top]` | 收藏列表/ID/导出/Top |
| GET | `/trash` | 回收站 |
| POST | `/trash/restore` | 批量还原 |
| POST | `/trash/empty` | 清空回收站 |
| POST/DELETE | `/trash/:id/restore` | 单个还原/彻底删除 |
| POST | `/views/:asset_id` | 记录浏览次数 |
| GET | `/persons` | 人物（人脸聚类）列表 |
| GET | `/persons/merge-suggestions` | 合并建议 |
| POST | `/persons/merge-suggestions/reject` | 拒绝合并建议 |
| POST | `/persons/merge` | 合并人物 |
| POST | `/persons/recluster` | 重新聚类 |
| GET/PUT/DELETE | `/persons/:id` | 人物详情/更新/删除 |
| POST | `/persons/:id/restore` | 恢复已删人物 |
| GET | `/persons/:id/assets` | 人物照片 |
| GET | `/persons/:id/relations` | 关联人物 |
| GET | `/persons/:id/places` | 人物出现的地点 |
| GET | `/persons/:id/face-thumbnail` | 人物头像缩略图（JWT 豁免） |
| POST | `/persons/:id/detach` | 从人物中移出特定脸 |
| GET | `/status` | 索引状态（pending/indexed/queue 数） |
| POST | `/scan` | 手动触发目录扫描 |
| GET | `/tasks` | 当前任务列表（index/embedding/ocr） |
| GET/PUT | `/config` | 相册配置（WatchDirs、功能开关等） |
| GET | `/storage` | 存储统计 |
| POST | `/cache/prune` | 清理孤儿缩略图 |
| POST | `/index/rebuild` | 重建向量索引 |
| GET | `/about` | 版本/ML 状态信息 |
| GET/POST/PUT/DELETE | `/smart-views[/:id]` | 语义自动相册 CRUD |
| POST | `/smart-views/preview` | 预览 Smart View 结果 |
| GET | `/smart-views/:id/assets` | Smart View 资产列表 |
| GET | `/smart-views/:id/activity` | Smart View 动态 |
| POST | `/smart-views/:id/export` | 导出 Smart View |
| POST | `/smart-views/:id/duplicate` | 复制 Smart View |

**TUS 路由**（注册在 `/v1/upload-tus`，独立于上述 group）：
- `ANY /v1/upload-tus` 和 `ANY /v1/upload-tus/*`：TUS v2 断点续传（POST/PATCH/HEAD/OPTIONS），单文件最大 20 GB，暂存目录 `/DATA/.system_data/photos-tus-staging`，7 天自动清理。

---

## 核心流程

### 1. 照片入库（扫描 / 监视）

```
fsnotify (Watcher)
  Create/Write → isSupportedMedia → Indexer.Enqueue(path)

ScanDirectory (手动/启动) → walkSupported → processFile (串行)

TUS 上传完成 → MarkAndReserve + rename → SubmitReserved
```

扫描范围是黑名单而非白名单（`service/scanroots.go` `isUserPartition`）：`/media` 或 `/mnt` 下任意挂载点默认都会被扫描，但 `/media/devmon/<卷标>`（devmon 自动挂载的可移动 U 盘/读卡器）被产品决策整体排除——不扫描、不被 MountGuard 追踪 offline，且启动时会硬删（含 CLIP 向量、缩略图）任何历史遗留的 devmon 资产；RAID（`/media/RAID_*`）、单盘 storage（`/mnt/Disk-*`）、MergerFS 等仍正常纳入。已知限制：若 devmon 被禁用/卸载，同一块 U 盘可能被 LocalStorage 抢挂到 `/mnt/Disk-*`，届时会被当作普通固定盘重新扫描。

**processFile 流水线（`service/indexer.go`）：**

1. 读取文件 → SHA-256 去重（`status='indexed'` 时跳过）
2. MIME 检测 → 判断图片/视频
3. **图片**：`goexif` 解析 EXIF（拍摄时间、GPS、相机型号、ISO、光圈、快门等）
4. **视频**：`ffprobe` 提取时长、分辨率、编解码器、帧率、码率、旋转、创建时间、GPS；再用 `ffmpeg` 提取关键帧用于后续 ML
5. INSERT/UPDATE `assets` + `asset_exif`（状态 `'pending'`）
6. 生成缩略图（small 250px / large 1280px，`disintegration/imaging`）
7. ML 推理（ML 服务就绪时）：
   - **CLIP 图像嵌入**：`nllb-clip-large-siglip__v1`（多语言，SigLIP SO400M 图像塔），1152 维，用 small.jpg 缩略图计算（与用户看到的帧一致），写入 `clip_embeddings`（sqlite-vec vec0 虚拟表）
   - **人脸检测+识别**：`antelopev2`（InsightFace ResNet100@Glint360K），512 维，写入 `face_detections`
   - **OCR**：`PP-OCRv5_server`，置信度 ≥0.5 的文字行写入 `asset_ocr`
8. 更新状态 `'indexed'`

### 2. 语义搜索（CLIP）

```
POST /search/smart
  → CLIPTextEmbed(query) → 1152 维查询向量
  → sqlite-vec KNN: "WHERE clip_embeddings MATCH ? AND k = ?"
  → 按 cosine distance 排序
  → （可选）OCR 精确匹配结果插队至顶部
  → 附加人脸名 + 地理名称
```

Smart View 也复用 `SmartSearch` 接口，按自然语言条件定义动态相册。

### 3. 人脸聚类

`FaceService.StartScheduler` 定期/触发聚类：
- DBSCAN（`epsilon=0.6, minPoints=1`），对 512 维余弦距离聚类
- 孤立脸吸附到已有 person 质心（`assignEpsilon=0.55`）
- 合并建议阈值 `suggestEpsilon=0.75`
- 结果写入 `persons` + `face_person`

### 4. Embedder 补跑（Backfill）

`Embedder.Run` 每 30 秒检测 ML 就绪状态：
- **false → true** 跳变时异步触发：
  1. `Backfill`：为所有 `status='indexed'` 但缺 CLIP 向量的资产补跑嵌入
  2. `reembedThumbnailsOnce`：一次性从缩略图重新计算所有已有嵌入（标记文件 `.clip_reembed_thumb_v1.done` 防止重复）
  3. `BackfillOCR`：为所有缺 OCR 文本的资产补跑

### 5. 地理反编码

`GeoService` 从 `asset_exif` 读取 GPS 坐标，用内嵌 Gazetteer（`pkg/geo/data/*.tsv.gz`：城市 15000+、国家、POI）做离线反编码，写入 `asset_geo`。Gazetteer 版本（`geoGazVersion`）变更时自动清空 `asset_geo` 并重跑。

### 6. ML 模型代次自动重建

`common.MLModelGen`（当前 `"2"`）标识当前二进制绑定的模型组合（CLIP/人脸/OCR 三个选型 + 维度）；成功重建后写入 `photos_meta.ml_model_gen`。服务启动时若检测到该键缺失（老库）或与当前 `MLModelGen` 不符，`Rebuilder.MaybeAutoRebuild`（`service/rebuild.go`）会轮询等待 ML 后端就绪（新模型缓存就位）后自动触发一次全量重建：清空 `clip_embeddings`/`asset_clip_idx`、对所有 `status='indexed'` 资产重跑 CLIP/人脸/OCR、重新聚类人脸并清理无脸的空 `persons`，最后写回新代次。空库（无资产）跳过 worker pool，直接完成重聚类并写代次，秒级完成。代次仅在重建成功收尾（`finalize()`）后写入，中途失败或断电会在下次启动时重试。

---

## 数据存储

```
/etc/nimoos/photos.conf          配置（INI，Viper 读取）
/DATA/.system_data/photos/       DataPath（默认）
  ├── photos.db                  SQLite 数据库（WAL 模式）
  ├── thumbs/<asset_id>/
  │     ├── small.jpg            250px 缩略图（CLIP 嵌入来源）
  │     └── large.jpg            1280px 缩略图
  ├── live/                      Live Photo 视频片段缓存
  └── ml-cache/                  immich-ml 模型缓存（bind-mount 进容器）
/DATA/.system_data/photos-tus-staging/   TUS 上传暂存（7 天清理）
/DATA/Gallery/                   默认照片主目录（可配置）
/var/run/nimoos/photos.url       服务发现地址
/var/log/nimoos/                 日志（zap）
```

### SQLite 主要表

| 表 | 用途 |
|---|---|
| `assets` | 资产主表（路径、MIME、拍摄时间、checksum、状态、软删除） |
| `asset_exif` | EXIF/视频元数据（分辨率、GPS、相机、ISO、编解码等） |
| `clip_embeddings` | sqlite-vec **vec0** 虚拟表，1152 维 CLIP 向量 |
| `asset_clip_idx` | rowid ↔ asset_id 映射（连接 clip_embeddings 与 assets） |
| `face_detections` | 人脸检测结果（bbox、512 维嵌入、excluded 标志） |
| `persons` | 人脸聚类结果（名称、封面、质心、置信度） |
| `face_person` | 人脸 → 人物映射 |
| `asset_ocr` | OCR 文本（coverage、line_count，用于区分文档与普通照片） |
| `asset_geo` | 反编码地理信息（城市、国家、geonameid） |
| `albums` + `album_assets` | 手动相册（支持排序） |
| `asset_favorites` | 按用户收藏 |
| `asset_views` | 按用户浏览计数 |
| `smart_views` + `smart_view_matches` | 语义自动相册及其匹配结果 |
| `merge_rejections` | 被拒绝的人脸合并建议对 |
| `place_cover_overrides` | 用户自定义地点封面 |
| `spot_name_overrides` | 用户自定义打点名称 |
| `photos_meta` | 键值元数据（如 `index_last_rebuilt`、`ml_model_gen`） |

---

## CGO 依赖

| 依赖 | 用途 |
|---|---|
| `mattn/go-sqlite3` | CGO SQLite3 驱动，需要系统 `gcc` + `sqlite3.h` |
| `asg017/sqlite-vec-go-bindings/cgo` | sqlite-vec 扩展（`vec0` 虚拟表），注入 `init()` 自动注册 |

构建必须 `CGO_ENABLED=1`，系统需安装 `gcc` 和 `libsqlite3-dev`（Debian/Ubuntu）或等价包。

```bash
CGO_ENABLED=1 go build -o nimoos-photos .
```

---

## 鉴权

JWT 校验（ECDSA P-256，公钥从 `/var/run/nimoos/` 读取），以下路径**豁免**：
- OPTIONS 请求（CORS preflight，TUS 客户端发送）
- `*/thumbnail`、`*/face-thumbnail`、`*/original`、`*/live`（媒体文件，`<img>` 标签无法附带 Authorization）
- `*/favorites/export`（同上）
- `POST */search/smart`，且请求来自 `127.0.0.1.*`（允许 NimoOS-AI Agent 内部调用）

校验通过后，JWT Claims 的用户 ID 以 `X-NimoOS-User-ID` Header 注入后续处理。

---

## 配置样例（`/etc/nimoos/photos.conf`）

```ini
[common]
RuntimePath = /var/run/nimoos
LogPath = /var/log/nimoos

[photos]
DataPath = /DATA/.system_data/photos
MLEndpoint = http://127.0.0.1:3003
Workers = 3
WatchDirs = /DATA/Gallery,/DATA/Documents,/DATA/Downloads
FacesEnabled = true
ScenesEnabled = true
OCREnabled = true
SmartViewEnabled = true
```

- `WatchDirs`：逗号分隔，fsnotify 监视目录，默认三个；可运行时通过 `PUT /v1/photos/config` 热生效（`Watcher.Restart`）。
- `Workers`：Indexer 并发 worker 数，默认 3。
- `MLEndpoint`：immich-machine-learning 地址，默认 `http://127.0.0.1:3003`。
- 各功能开关（`FacesEnabled/ScenesEnabled/OCREnabled/SmartViewEnabled`）缺省均为 `true`；关闭 `ScenesEnabled` 后新照片不再生成 CLIP 向量，语义搜索将失效。

---

## 与其他服务的依赖

| 服务 | 关系 |
|---|---|
| **NimoOS-Gateway** | 启动时 `POST /v1/gateway/routes` 注册三个前缀（`/v1/photos`、`/doc/v1/photos`、`/v1/upload-tus`）；从 `RuntimePath` 读取 Gateway 地址 |
| **NimoOS-UserService** | 从 `/var/run/nimoos/` 读取 ECDSA 公钥校验 JWT |
| **NimoOS-MessageBus** | systemd `After=nimoos-message-bus.service`；`service/publisher.go` 向 MessageBus 发布事件（索引进度等） |
| **immich-machine-learning** | Docker 容器，`127.0.0.1:3003`，`pkg/mlclient` 通过 `/predict` + multipart/form-data 调用；ML 不可用时 CLIP/人脸/OCR 步骤跳过，Embedder 检测到恢复后自动补跑 |
| **NimoOS-AI Agent** | 可无 JWT 从 localhost 调用 `POST /search/smart`，作为 AI Agent 文件搜索后端 |

---

## 已知坑

1. **inotify 配额放大**：NimoOS-Photos 的 Watcher、Wiki（`NimoOS/`）和 NimoOS-Search 三个 fsnotify 实例都监视 `/DATA/Gallery` 等目录。每个实例独立占用 inotify watches，导致 `/proc/sys/fs/inotify/max_user_watches` 压力叠加。若目录树超大，需手动调高配额（如 `echo 524288 > /proc/sys/fs/inotify/max_user_watches`）。当前维持各自独立监视（方案 A），统一共享监视层为后续待办。

2. **ML 服务离线**：ML 容器（immich-machine-learning）为离线镜像捆绑包，首次需通过安装脚本 `docker load`。容器未启动时 CLIP 嵌入、人脸识别、OCR 全部跳过（`ml.IsReady()` 返回 false）；Embedder 检测到 `false→true` 跳变时自动补跑历史资产。

3. **视频缩略图用于 CLIP 嵌入**：视频用 ffmpeg 提取的关键帧生成 CLIP 嵌入（而非关键帧原图），与图片路径统一为 `small.jpg`；这是有意设计——避免高细节关键帧在语义搜索中不当排名高于图片。标记文件 `.clip_reembed_thumb_v1.done` 防止重建索引后的重复嵌入。

4. **TUS 上传与 fsnotify 竞态**：TUS 上传完成后先 `MarkAndReserve` 占位，再 rename，最后 `SubmitReserved`，防止 Watcher 的 Create 事件抢先走匿名 batch 槽（`batches[""]`），导致前端进度报告错乱。

---

## 启动顺序与部署

systemd 依赖（见 `build/sysroot/usr/lib/systemd/system/nimoos-photos.service`）：

```
nimoos-message-bus.service ──▶ nimoos-photos.service
```

`Type=notify`，`SdNotify(Ready)` 后才视为启动完成。

```bash
# 构建
CGO_ENABLED=1 go build -o nimoos-photos .

# 部署（替换二进制并重启）
bash nimo_os_docs/scripts/deploy.sh photos
```

ML 后端独立部署（`deploy/ml/`），按核显厂商分 **openvino**（Intel）/ **rocm**（AMD，含 gfx1151 / AI Max+ 395 Strix Halo 的 `HSA_OVERRIDE_GFX_VERSION` 覆盖）两个离线分发包 flavor：

- **打包**（`script/package-photos-ml.sh <openvino|rocm|cpu> [输出目录]`，在有外网的机器上执行一次）：拉取对应 immich-machine-learning 官方镜像 tag 并重打本地标签、启动临时容器预热 CLIP 图/文塔、人脸、OCR 三个模型缓存（模型名从 `common/constants.go` 用正则抓取，保证打包与代码选型一致），再把镜像 tar、模型缓存 tar、`docker-compose.yml`、`install.sh`、`overrides/` 打成一个分发压缩包。
- **安装**（`install.sh`，幂等，重复运行=更新到包内镜像版本）：读取包内 `FLAVOR` 文件，用 `/sys/class/drm/card*/device/vendor`（Intel=`0x8086`、AMD=`0x1002`）自动识别本机核显厂商并与 flavor 校验（不匹配则拒绝安装）；按 flavor 把 `overrides/<flavor>.yml` 拷贝为 `docker-compose.override.yml` 叠加设备直通（`/dev/dri`；AMD 再加 `/dev/kfd` + `HSA_OVERRIDE_GFX_VERSION`）；随包模型缓存解压进 `ml-cache`（已存在则跳过，`FORCE_MODELS=1` 强制覆盖）；`docker compose up -d` 后轮询 `/ping` 确认就绪。
- **运行时环境变量**（`docker-compose.yml`）：`MACHINE_LEARNING_MODEL_TTL=300` 秒闲置自动卸载模型释放内存（nllb-large + PP-OCRv5_server 常驻内存/显存开销较大）、`MACHINE_LEARNING_MODEL_TTL_POLL_S=10`、`HF_HUB_OFFLINE=1` 禁止运行时联网查 HuggingFace（模型缓存已随包预置，离线/内网机器联网查询会卡超时）。

```bash
# 打包（一次性，需要外网）
script/package-photos-ml.sh openvino   # 或 rocm / cpu

# 目标机器：解压分发包后安装/更新（幂等）
tar -xzf photos-ml-openvino-v2.7.5.tar.gz -C /tmp/photos-ml
/tmp/photos-ml/install.sh
```
