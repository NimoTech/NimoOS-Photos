# Photos 全盘扫描 + 周期/手动重扫 + 设置项 —— 设计文档

> 日期：2026-06-09 ｜ 范围：NimoOS-Photos（Go 后端）+ NimoOS-UI（Photos 设置前端） ｜ 状态：已与用户确认，待落实现计划

## 目标

1. **扩大扫描范围**：Photos 索引所有"用户可接触分区"的全部文件夹——系统盘 `/DATA` 下所有用户文件、组 RAID 产生的 `/media/RAID_*` 分区、手动挂载盘 `/media/Storage_*`、以及后续插入的 U 盘（udev 自动挂载在 `/mnt/Disk-*`）。只把白名单扩展名文件入库，继续跳过 `.` 开头的隐藏文件夹。
2. **手动重扫**：在 Photos 设置里加一个符合现有设计的"立即重扫"按钮，用户可手动触发全量重扫。
3. **周期重扫**：默认每 24 小时自动重扫一次，并在设置里提供间隔档位供用户选择（含"关闭"）。

## 已确认的关键决策

- **分区枚举**：Photos 本地直读 `/proc/self/mountinfo`（复用 `github.com/moby/sys/mountinfo`）自行筛选，不调 LocalStorage API、不依赖 Gateway/JWT、不订阅 MessageBus。
- **U 盘热插拔**：靠周期重扫 + 手动重扫覆盖，**不**实现 MessageBus 实时订阅。U 盘插入后于下一轮周期或用户手动点击时被扫入。
- **/DATA 范围**：递归扫整个 `/DATA`，排除系统目录 `.system_data`（`.` 开头已被现有逻辑跳过）、`AppData`、`lost+found`，以及所有 `.` 开头隐藏目录。
- **周期档位**：关闭 / 6h / 12h / 24h（默认）/ 7 天。

## 现状（实现起点）

- 扫描引擎 `Indexer.ScanDirectory(dir)`：`filepath.WalkDir` 递归，跳过 `.` 开头子目录（`service/indexer.go:894-898`），按白名单扩展名过滤（`service/indexer.go:48-66`），结尾 `pruneMissingUnder(dir)` 清除已删文件的 DB 记录。
- 触发路径：① 启动时对每个 `WatchDirs` 跑 `ScanDirectory`（`service/service.go:147-153`）；② fsnotify 监听 WatchDirs（非递归，`service/watcher.go:49-88`）；③ TUS 上传完成入队（`route/v1/tus.go`）；④ `POST /v1/photos/scan` 手动扫**硬编码**的 `/DATA/Gallery,/DATA/Documents,/DATA/Downloads`（`route/v1/index.go:47-54`）。
- 配置：`photos.conf` 的 `[photos]` 段，字段含 `WatchDirs`、`RetentionDays`、`FacesEnabled` 等（`pkg/config/config.go:17-29`）；**无任何扫描间隔字段**。`config.Save()` 用 viper 写回 ini（`pkg/config/config.go:121-156`）。`GET/PUT /v1/photos/config`（`route/v1/config.go`）。
- 服务内已有 24h ticker 模式可参考（`main.go:88-92` TUS 清理、`service/service.go:194-204` 回收站 purge）。
- 挂载点规律：RAID=`/media/RAID_<名>`（`LocalStorage raid.go:265`）、手动挂载盘=`/media/Storage_*`（`disk_methods.go:9`）、udev 自动挂载 U 盘分区=`/mnt/Disk-<UUID8位>`（`disk.go:803`）。
- 前端 `PhotosSettings.vue`（441 行）用自有设计系统 `.st-card/.st-row/.st-row-text/.st-row-label/.st-row-desc/.st-segmented/.st-btn/.st-divider`；Storage 卡片已有"回收站保留"（分段按钮）与"缩略图缓存"（按钮）两行可作模板。`src/service/photos.js` 已有 `getConfig()`、`updateConfig(watchDirs, retentionDays, facesEnabled, extra={})`、`triggerScan()`。

## 设计

### A. 后端扫描引擎

**`enumerateScanRoots() []string`（新函数）**：
- 始终包含 `/DATA`。
- 读 `/proc/self/mountinfo`（`mountinfo.GetMounts`），收集挂载点 `mp`，满足 `strings.HasPrefix(mp, "/media/")` 或 `strings.HasPrefix(mp, "/mnt/Disk-")` 的加入结果。
- 去重、排序，返回。
- 枚举失败 → 仅返回 `["/DATA"]` 并记 warn。

**`/DATA` 排除集**：`ScanDirectory` 接受可选排除（或内部针对 `/DATA` 顶层判断），跳过名为 `AppData`、`lost+found` 的目录；`.system_data` 及其它 `.` 开头目录由现有 `.`-skip 覆盖。`/media/*`、`/mnt/Disk-*` 仅沿用 `.`-skip，不另加排除。

**白名单扩展名、`.`-skip、`pruneMissingUnder` 不变**。

**统一三个全量触发点走 `enumerateScanRoots()`**：
- 启动全扫（`service.go`）：遍历 roots `ScanDirectory`。
- 周期 ticker（新增，见 B）。
- 手动 `POST /v1/photos/scan`（`route/v1/index.go`）：把硬编码 3 目录替换为 `enumerateScanRoots()`。
- fsnotify 对 `WatchDirs` 的实时监听保留不动。

### B. 配置 + 周期调度

- `Config`/`Settings` 新增 `ScanInterval int`（分钟，0=关闭，默认 1440）；`photos.conf` `[photos] ScanInterval`；`config.go` 默认值 + `Save()` 写回 `v.Set("photos.ScanInterval", ...)`。
- 服务新增周期 goroutine：`ScanInterval>0` 时 `time.NewTicker(time.Duration(ScanInterval)*time.Minute)`，到点遍历 `enumerateScanRoots()` 逐一 `ScanDirectory`（出错记日志、跳过、继续）；`==0` 不启动。
- `RestartScanTicker(minutes int)`（类比 `RestartWatcher`）：停旧 ticker，按新值重建（0 则停用）。
- `GET /v1/photos/config` 返回新增 `scanInterval`；`PUT /v1/photos/config` 接收 `scanInterval`（校验 ∈ {0,360,720,1440,10080}），`Save` 后调 `RestartScanTicker`。

### C. 前端 UI（PhotosSettings.vue，复用 `.st-*`，零新样式）

在 Storage 卡片 `#storage` 内、"回收站保留"行附近新增两行（用 `.st-row` + `.st-row-text`/`.st-row-label`/`.st-row-desc`）：

1. **立即重扫图库**：右侧 `.st-btn`（复用 clearCache 的 busy/spinner 三态）→ `photos.triggerScan()` → 成功弹 toast（`$buefy.toast`，is-info）。
2. **自动重扫间隔**：右侧 `.st-segmented` 分段按钮 `关闭/6h/12h/24h/7天`，值映射分钟 `0/360/720/1440/10080`，绑定响应式 `scanInterval`，变更经 `photos.updateConfig(this.watchDirs, this.retention, this.facesEnabled, { scanInterval })` 存盘（沿用现有 `updateConfig` 的 `extra` 通道）。

- 初始值从 `getConfig()` 读 `scanInterval`。
- 所有新文案走 `$t`（至少 en_US + zh_CN，其余回退 en）。新增 i18n key：
  - `photos_rescan_now`（按钮）/ `photos_rescan_now_desc`
  - `photos_scan_interval`（标题）/ `photos_scan_interval_desc`
  - 档位标签 `photos_scan_off` / `photos_scan_6h` / `photos_scan_12h` / `photos_scan_24h` / `photos_scan_7d`（或用通用时间格式）
  - 重扫成功提示 `photos_rescan_started`

### D. 边界与错误处理

- **U 盘拔出不删照片**：`pruneMissingUnder` 只对当前枚举到（仍挂载）的根执行；拔出的盘不在 roots → 不扫 → 其索引条目保留（再插回即恢复，拔出期间缩略图可能失效）。刻意如此，避免拔盘清空照片。
- 挂载枚举失败 → 退化为仅扫 `/DATA`，warn 日志，不中断。
- 周期扫某根出错（盘忙/权限）→ 记日志、跳过、继续其余根。
- 手动扫返回 202（异步后台），进度走现有 `indexStatus`。
- 大盘/慢 U 盘扫描在后台 worker 串行，不阻塞 API。

### 不做（YAGNI）

- 不实现 MessageBus 实时订阅（U 盘靠周期+手动）。
- 不改 fsnotify 为递归（外接盘/深层目录靠周期补）。
- 不改动 `WatchDirs` 既有语义与实时监听。

## 测试

- **后端**：`enumerateScanRoots` 单测（mock /proc/mountinfo 或注入挂载列表：含 `/media/RAID_x`、`/mnt/Disk-x`、应排除 `/`、`tmpfs`、`/proc`）；`/DATA` 排除集单测（`AppData`/`lost+found`/`.system_data` 被跳过，`Documents` 等保留）；config 读写含 `ScanInterval` 的往返；`RestartScanTicker(0)` 停用、`>0` 重建。
- **前端**：`scanInterval` 分钟↔档位映射；`updateConfig` 携带 `scanInterval`；按钮 busy 态。
- **回归**：现有 Photos 后端测试与 UI 测试全绿。
- **手动验收**：设置里点"立即重扫"→ U 盘/RAID 里的白名单文件进库；改间隔为 6h 并持久化（重启后仍生效）；拔 U 盘后照片仍在、插回恢复；`/DATA/AppData`、`.system_data` 不被索引。
