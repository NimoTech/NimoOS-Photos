# Photos 全盘扫描 + 周期/手动重扫 + 设置项 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development。Steps 用 `- [ ]`。跨两仓：NimoOS-Photos（Go 后端）+ NimoOS-UI（前端），按仓分轨。

**Goal:** Photos 索引所有用户可接触分区（/DATA 用户文件 + /media/* RAID/挂载盘 + /mnt/Disk-* U盘）的白名单文件，并提供手动"立即重扫"按钮与可配置的周期重扫（默认 24h）。

**Architecture:** 后端新增 `EnumerateScanRoots()`（直读 /proc/mounts 自筛 /media//mnt/Disk- + /DATA），三个触发点（启动/周期 ticker/手动 API）统一走它；`/DATA` 额外排除 AppData/lost+found（.system_data 已被 .-skip）。新增 config 字段 `ScanInterval`（分钟，0=关）+ 周期 goroutine + `RestartScanTicker`。前端在 Photos 设置 Storage 卡片加"立即重扫"按钮 + 间隔分段选择器，复用现有 `.st-*` 设计、零新样式。

**Tech Stack:** Go（echo、viper、SQLite）；Vue 2 + Vuex + 自有 `.st-*` 设计系统 + vue-i18n；测试 Go `go test` / 前端 Vitest。

**参考 spec：** `NimoOS-Photos/docs/superpowers/specs/2026-06-09-photos-full-disk-scan-design.md`

**关键既有事实：**
- `service/indexer.go`：`walkSupported(dir, fn)` 递归、跳过 `.` 开头目录（行 894-898）、白名单扩展名（行 48-66）；`ScanDirectory(dir) error` 串行扫 + 末尾 `pruneMissingUnder(dir)`。
- `service/service.go`：`NewService` 末尾 `return &services{...}`；启动扫描 goroutine（行 147-153）`for _, dir := range cfg.WatchDirs { idx.ScanDirectory(dir) }`；已有 24h ticker 模式（行 183-205）；`RestartWatcher`（行 228-232）；`Services` 接口（行 ~21-37）；`services` 结构有 `parentCtx`、`indexer`。
- `route/v1/index.go` `Scan`：硬编码 `for _, dir := range []string{"/DATA/Gallery","/DATA/Documents","/DATA/Downloads"}`（行 47-54）。
- `route/v1/config.go`：GET 返回 watchDirs/retentionDays/facesEnabled/... ；PUT req 用 `*bool` 区分"未传/传"，校验 watchDirs 非空、retentionDays 1-365，`config.Save(Settings{...})` 后 `RestartWatcher`。
- `pkg/config/config.go`：`Config` 结构、`Init`（用 `v.IsSet("photos.X")` 判缺省赋默认）、`Settings` 结构、`Save`（viper 写 `.ini` 临时文件再 rename）。`config_test.go` 已存在。
- 前端 `src/service/photos.js`：`updateConfig(watchDirs, retentionDays, facesEnabled, extra={})`，extra 白名单**仅** `['scenesEnabled','ocrEnabled','smartViewEnabled']`（行 28-31）；`triggerScan()`→POST /scan；`getConfig()`。
- `src/store/modules/photos.js`：`setTrashRetention(days)`（getConfig 取 watchDirs → updateConfig 回传，行 1054-1058）、`fetchTrashRetention`、`setAiFeatures`（行 980-991）为镜像模板。
- `src/views/Photos/PhotosSettings.vue`：Storage 卡片 `#storage`（行 39-102）含 retention 分段行（行 78-88）+ 缩略图缓存按钮行（行 90-101）；`data()`（行 204）；`showToast(icon,text)`（行 399-403）；`mounted`（行 412）；`busy` 态。用自有 `.st-row/.st-segmented/.st-btn` 类。

---
---

## 轨道一：NimoOS-Photos 后端（cd /DATA/.nimoos-dev/NimoOS-Photos）

测试：`go test ./pkg/config/ -run X -v`、`go test ./service/ -run X -v`。提交身份 `wiwiwilliam <yuwu0321@gmail.com>`，不加 Co-Authored-By，不 push。

### Task B1: EnumerateScanRoots（直读 /proc/mounts 自筛）

**Files:** Create `service/scanroots.go`；Test `service/scanroots_test.go`

- [ ] **Step 1: 写失败测试** `service/scanroots_test.go`：

```go
package service

import (
	"reflect"
	"testing"
)

func TestParseScanRoots(t *testing.T) {
	mounts := `/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw,nosuid 0 0
tmpfs /run tmpfs rw,nosuid 0 0
/dev/sda2 /DATA ext4 rw,relatime 0 0
/dev/md0 /media/RAID_Photos ext4 rw 0 0
/dev/sdb1 /media/Storage_usb0 vfat rw 0 0
/dev/sdc1 /mnt/Disk-1a2b3c4d ext4 rw 0 0
/dev/sdd1 /media/RAID_With\040Space ext4 rw 0 0
`
	got := parseScanRoots(mounts)
	want := []string{
		"/DATA",
		"/media/RAID_Photos",
		"/media/RAID_With Space",
		"/media/Storage_usb0",
		"/mnt/Disk-1a2b3c4d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseScanRoots\n got=%v\nwant=%v", got, want)
	}
}

func TestParseScanRootsAlwaysIncludesDATAOnce(t *testing.T) {
	// 即使 /proc/mounts 没有 /DATA 行，也要包含 /DATA，且只一次。
	got := parseScanRoots("proc /proc proc rw 0 0\n")
	if len(got) != 1 || got[0] != "/DATA" {
		t.Fatalf("want [/DATA], got %v", got)
	}
}
```

- [ ] **Step 2: 跑确认失败** `go test ./service/ -run TestParseScanRoots -v` → 编译失败（parseScanRoots 未定义）。

- [ ] **Step 3: 实现** `service/scanroots.go`：

```go
package service

import (
	"os"
	"sort"
	"strings"
)

// procMountsPath is the kernel mount table; overridable in tests (unused in
// prod). EnumerateScanRoots reads it to discover user-accessible partitions.
const procMountsPath = "/proc/mounts"

// EnumerateScanRoots returns every directory Photos should scan: the system
// disk (/DATA) plus every currently-mounted user partition under /media/*
// (RAID arrays, manually-mounted drives) and /mnt/Disk-* (udev auto-mounted
// USB). On read failure it degrades to just /DATA.
func EnumerateScanRoots() []string {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return []string{"/DATA"}
	}
	return parseScanRoots(string(data))
}

// parseScanRoots is the pure, testable core of EnumerateScanRoots.
func parseScanRoots(mounts string) []string {
	set := map[string]bool{"/DATA": true}
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mp := unescapeMount(fields[1])
		if strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/Disk-") {
			set[mp] = true
		}
	}
	roots := make([]string, 0, len(set))
	for mp := range set {
		roots = append(roots, mp)
	}
	sort.Strings(roots)
	return roots
}

// unescapeMount decodes the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses in mount-point fields.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
```

- [ ] **Step 4: 跑确认通过** `go test ./service/ -run TestParseScanRoots -v` → PASS（2 个）。

- [ ] **Step 5: 提交**
```bash
git add service/scanroots.go service/scanroots_test.go
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(scan): 新增 EnumerateScanRoots，直读 /proc/mounts 自筛用户分区"
```

---

### Task B2: /DATA 系统目录排除

**Files:** Modify `service/indexer.go`（walkSupported）；Test `service/indexer_scanexclude_test.go`

- [ ] **Step 1: 写失败测试** `service/indexer_scanexclude_test.go`：

```go
package service

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// walkSupported must skip /DATA/AppData and /DATA/lost+found entirely, while
// still descending into ordinary user folders. (.system_data is covered by the
// existing dot-prefix skip and is not retested here.)
func TestWalkSupportedSkipsDATASystemDirs(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "DATA")
	// stand in for the absolute paths the exclusion set matches
	oldExcl := scanExcludeDirs
	scanExcludeDirs = map[string]bool{
		filepath.Join(data, "AppData"):   true,
		filepath.Join(data, "lost+found"): true,
	}
	defer func() { scanExcludeDirs = oldExcl }()

	mk := func(rel string) {
		p := filepath.Join(data, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("Documents/a.jpg")
	mk("AppData/app/b.jpg")
	mk("lost+found/c.png")

	var got []string
	if err := walkSupported(data, func(p string) {
		rel, _ := filepath.Rel(data, p)
		got = append(got, rel)
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"Documents/a.jpg"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
```

- [ ] **Step 2: 跑确认失败** `go test ./service/ -run TestWalkSupportedSkipsDATASystemDirs -v` → 编译失败（scanExcludeDirs 未定义）或断言失败。

- [ ] **Step 3: 实现** 在 `service/indexer.go` 顶部（紧邻 `supportedExts` 定义附近）新增包级变量：

```go
// scanExcludeDirs are absolute directory paths excluded from scanning even
// though their names don't start with ".". They hold app/system data, not
// user media. (.system_data is already skipped by the dot-prefix rule.)
var scanExcludeDirs = map[string]bool{
	"/DATA/AppData":    true,
	"/DATA/lost+found": true,
}
```

并修改 `walkSupported` 的目录分支：把
```go
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
```
改为：
```go
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if scanExcludeDirs[path] {
				return filepath.SkipDir
			}
			return nil
		}
```

- [ ] **Step 4: 跑确认通过** `go test ./service/ -run TestWalkSupportedSkipsDATASystemDirs -v` → PASS。

- [ ] **Step 5: 提交**
```bash
git add service/indexer.go service/indexer_scanexclude_test.go
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(scan): /DATA 扫描排除 AppData / lost+found 系统目录"
```

---

### Task B3: 配置字段 ScanInterval

**Files:** Modify `pkg/config/config.go`；Test `pkg/config/config_test.go`（追加）

- [ ] **Step 1: 写失败测试** 在 `pkg/config/config_test.go` 追加：

```go
func TestScanIntervalDefaultAndSave(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "photos.conf")
	sample := "[photos]\nWatchDirs = /DATA/Gallery\n"
	if err := Init(cf, sample); err != nil {
		t.Fatal(err)
	}
	// 配置无 ScanInterval key 时默认 1440（24h）
	if Cfg.ScanInterval != 1440 {
		t.Fatalf("default ScanInterval=1440, got %d", Cfg.ScanInterval)
	}
	// Save 持久化并能被重新 Init 读回
	if err := Save(Settings{WatchDirs: []string{"/DATA/Gallery"}, ScanInterval: 360}); err != nil {
		t.Fatal(err)
	}
	if Cfg.ScanInterval != 360 {
		t.Fatalf("after Save ScanInterval=360, got %d", Cfg.ScanInterval)
	}
	if err := Init(cf, sample); err != nil {
		t.Fatal(err)
	}
	if Cfg.ScanInterval != 360 {
		t.Fatalf("reloaded ScanInterval=360, got %d", Cfg.ScanInterval)
	}
}
```
（若文件未 import `path/filepath`，按现有 import 风格补上。）

- [ ] **Step 2: 跑确认失败** `go test ./pkg/config/ -run TestScanIntervalDefaultAndSave -v` → 编译失败（Cfg.ScanInterval / Settings.ScanInterval 不存在）。

- [ ] **Step 3: 实现** `pkg/config/config.go`：
(a) `Config` 结构加字段（在 `RetentionDays int` 下）：`ScanInterval int`。
(b) `Init` 里读取并赋默认（在 `Cfg = &Config{...}` 的字段列表加 `ScanInterval: v.GetInt("photos.ScanInterval"),`，并在末尾默认块加）：
```go
	// 扫描间隔（分钟）；配置无此 key 时默认 1440（24h）。0 = 关闭周期重扫。
	if !v.IsSet("photos.ScanInterval") {
		Cfg.ScanInterval = 1440
	}
```
(c) `Settings` 结构加字段：`ScanInterval int`。
(d) `Save` 里持久化（在 `v.Set("photos.SmartViewEnabled", ...)` 之后）：`v.Set("photos.ScanInterval", s.ScanInterval)`；并在末尾更新内存（在 `Cfg.SmartViewEnabled = ...` 之后）：`Cfg.ScanInterval = s.ScanInterval`。

- [ ] **Step 4: 跑确认通过** `go test ./pkg/config/ -run TestScanIntervalDefaultAndSave -v` → PASS。

- [ ] **Step 5: 提交**
```bash
git add pkg/config/config.go pkg/config/config_test.go
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(config): 新增 ScanInterval（分钟，默认1440，0=关）"
```

---

### Task B4: 周期 ticker + RestartScanTicker + 统一三触发点

**Files:** Modify `service/service.go`、`route/v1/index.go`

- [ ] **Step 1: service.go —— Services 接口加方法**

在 `Services` 接口里（`RestartWatcher(dirs []string)` 旁）加：
```go
	RestartScanTicker(minutes int)
```

- [ ] **Step 2: service.go —— services 结构加字段**

在 `services` 结构（含 `watcher`、`parentCtx` 的那个）加：
```go
	scanMu           sync.Mutex
	scanTickerCancel context.CancelFunc
```
（确保文件已 import `"sync"`；`context`、`time` 已 import。）

- [ ] **Step 3: service.go —— 实现 RestartScanTicker**

在 `RestartWatcher` 方法附近新增：
```go
// RestartScanTicker (re)starts the periodic full-disk scan loop. minutes<=0
// disables it. Each tick scans every root from EnumerateScanRoots(). Safe to
// call repeatedly (config changes); the previous loop is cancelled first.
func (s *services) RestartScanTicker(minutes int) {
	if s.indexer == nil {
		return // test services without an indexer
	}
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanTickerCancel != nil {
		s.scanTickerCancel()
		s.scanTickerCancel = nil
	}
	if minutes <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.parentCtx)
	s.scanTickerCancel = cancel
	go func() {
		ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, dir := range EnumerateScanRoots() {
					s.indexer.ScanDirectory(dir) //nolint:errcheck
				}
			}
		}
	}()
}
```

- [ ] **Step 4: service.go —— 启动扫描改用 EnumerateScanRoots，并启动 ticker**

(a) 把启动扫描 goroutine 里的：
```go
		for _, dir := range cfg.WatchDirs {
			idx.ScanDirectory(dir) //nolint:errcheck
		}
```
改为：
```go
		for _, dir := range EnumerateScanRoots() {
			idx.ScanDirectory(dir) //nolint:errcheck
		}
```

(b) 把结尾 `return &services{...}` 改为先赋值再启动 ticker再返回：
```go
	svc := &services{
		// ...原有字段全部保留不变...
	}
	svc.RestartScanTicker(cfg.ScanInterval)
	return svc
```
（即把原 `return &services{...}` 拆成 `svc := &services{...}` + `svc.RestartScanTicker(cfg.ScanInterval)` + `return svc`。）

- [ ] **Step 5: route/v1/index.go —— 手动 Scan 改用 EnumerateScanRoots**

把 `Scan` 里的：
```go
	go func() {
		for _, dir := range []string{"/DATA/Gallery", "/DATA/Documents", "/DATA/Downloads"} {
			h.svc.Indexer().ScanDirectory(dir) //nolint:errcheck
		}
	}()
```
改为：
```go
	go func() {
		for _, dir := range service.EnumerateScanRoots() {
			h.svc.Indexer().ScanDirectory(dir) //nolint:errcheck
		}
	}()
```
（确保 index.go 已 import `"github.com/NimoTech/NimoOS-Photos/service"`；它已用 `service.Services`，故已 import。）

- [ ] **Step 6: 编译 + 全后端测试**

Run: `go build ./... && go test ./service/ ./pkg/config/ -v 2>&1 | tail -20`
Expected: 编译通过，测试全绿（含 NewTestServices 不受影响——RestartScanTicker 对 nil indexer 直接 return）。

- [ ] **Step 7: 提交**
```bash
git add service/service.go route/v1/index.go
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(scan): 周期重扫 ticker + RestartScanTicker，启动/手动扫统一走 EnumerateScanRoots"
```

---

### Task B5: config GET/PUT 暴露 scanInterval

**Files:** Modify `route/v1/config.go`

- [ ] **Step 1: GET 增加 scanInterval**

在 `GetConfig` 返回的 map 里加一行：`"scanInterval": config.Cfg.ScanInterval,`。

- [ ] **Step 2: PUT 接收 + 校验 + 持久化 + 重启 ticker**

(a) `req` struct 加字段：`ScanInterval *int `json:"scanInterval"``。

(b) 在 retentionDays 校验之后，新增 scanInterval 处理：
```go
	scanInterval := config.Cfg.ScanInterval
	if req.ScanInterval != nil {
		switch *req.ScanInterval {
		case 0, 360, 720, 1440, 10080:
			scanInterval = *req.ScanInterval
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "scanInterval must be one of 0,360,720,1440,10080")
		}
	}
```

(c) 把 `config.Save(config.Settings{...})` 的入参加上 `ScanInterval: scanInterval,`。

(d) `config.Save` 成功、`h.svc.RestartWatcher(req.WatchDirs)` 之后，新增：`h.svc.RestartScanTicker(scanInterval)`。

(e) PUT 返回 map 里加：`"scanInterval": config.Cfg.ScanInterval,`。

- [ ] **Step 3: 编译 + handler 测试**

Run: `go build ./... && go test ./route/... -v 2>&1 | tail -15`
Expected: 编译通过；现有 config handler 测试仍绿（若有 NewTestServices，RestartScanTicker 为 nil-indexer no-op）。

- [ ] **Step 4: 提交**
```bash
git add route/v1/config.go
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(config): GET/PUT 暴露 scanInterval（校验档位 + 重启周期 ticker）"
```

---
---

## 轨道二：NimoOS-UI 前端（cd /DATA/.nimoos-dev/NimoOS-UI）

测试 `pnpm test <文件>`。提交身份同上。

### Task F1: photos.js —— updateConfig 白名单加 scanInterval

**Files:** Modify `src/service/photos.js`

- [ ] **Step 1: 改 extra 白名单**

把 `updateConfig` 里：
```js
    for (const k of ['scenesEnabled', 'ocrEnabled', 'smartViewEnabled']) {
```
改为：
```js
    for (const k of ['scenesEnabled', 'ocrEnabled', 'smartViewEnabled', 'scanInterval']) {
```

- [ ] **Step 2: lint** `pnpm lint src/service/photos.js` → 无新增报错。

- [ ] **Step 3: 提交**
```bash
git add src/service/photos.js
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(photos): updateConfig 支持透传 scanInterval"
```

---

### Task F2: store —— fetchScanInterval / setScanInterval action + state

**Files:** Modify `src/store/modules/photos.js`；Test `tests/photos-upload-store.test.js`（追加，或新建 `tests/photosScanInterval.test.js`）

- [ ] **Step 1: 写失败测试** 新建 `tests/photosScanInterval.test.js`：

```js
import { vi } from 'vitest'
import photos from '@/store/modules/photos.js'

vi.mock('@/service/photos.js', () => ({
  default: {
    getConfig: vi.fn(() => Promise.resolve({ data: { watchDirs: ['/DATA/Gallery'], retentionDays: 30, scanInterval: 1440 } })),
    updateConfig: vi.fn(() => Promise.resolve({ data: {} })),
  },
}))
import photosService from '@/service/photos.js'

describe('photos scanInterval action', () => {
  test('SET_SCAN_INTERVAL 写入 state', () => {
    const state = { scanIntervalMinutes: 1440 }
    photos.mutations.SET_SCAN_INTERVAL(state, 360)
    expect(state.scanIntervalMinutes).toBe(360)
  })

  test('setScanInterval：读现有 watchDirs 回传 + 透传 scanInterval', async () => {
    const commit = vi.fn()
    await photos.actions.setScanInterval({ commit }, 360)
    expect(photosService.getConfig).toHaveBeenCalled()
    expect(photosService.updateConfig).toHaveBeenCalledWith(['/DATA/Gallery'], 30, undefined, { scanInterval: 360 })
    expect(commit).toHaveBeenCalledWith('SET_SCAN_INTERVAL', 360)
  })
})
```

- [ ] **Step 2: 跑确认失败** `pnpm test tests/photosScanInterval.test.js` → FAIL（SET_SCAN_INTERVAL / setScanInterval 不存在）。

- [ ] **Step 3: 实现** `src/store/modules/photos.js`：
(a) state 里（`trashRetentionDays: 30` 附近）加：`scanIntervalMinutes: 1440,`。
(b) mutations 里加：
```js
    SET_SCAN_INTERVAL(state, minutes) { state.scanIntervalMinutes = minutes },
```
(c) actions 里（`setTrashRetention` 附近）加（镜像 setTrashRetention）：
```js
    async fetchScanInterval({ commit }) {
      try {
        const res = await photosService.getConfig()
        if (res?.data?.scanInterval != null) commit('SET_SCAN_INTERVAL', res.data.scanInterval)
      } catch (e) { console.warn('[photos] fetchScanInterval', e) }
    },
    // 设置页同管 watchDirs；改间隔时 watchDirs/retention 用当前值回传以满足后端非空校验。
    async setScanInterval({ commit }, minutes) {
      const res = await photosService.getConfig()
      const watchDirs = res?.data?.watchDirs || []
      const retention = res?.data?.retentionDays
      await photosService.updateConfig(watchDirs, retention, undefined, { scanInterval: minutes })
      commit('SET_SCAN_INTERVAL', minutes)
    },
```

- [ ] **Step 4: 跑确认通过** `pnpm test tests/photosScanInterval.test.js` → PASS（2）。

- [ ] **Step 5: 提交**
```bash
git add src/store/modules/photos.js tests/photosScanInterval.test.js
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(photos): store 新增 fetchScanInterval / setScanInterval"
```

---

### Task F3: i18n 文案

**Files:** Modify `src/assets/lang/en_US.json`、`src/assets/lang/zh_CN.json`

- [ ] **Step 1: en_US.json 追加键**（在结尾 `}` 前，注意前一行补逗号）：
```json
  "Rescan library": "Rescan library",
  "Scan all drives now and add new photos and videos to the library.": "Scan all drives now and add new photos and videos to the library.",
  "Rescan now": "Rescan now",
  "Rescanning…": "Rescanning…",
  "Library rescan started": "Library rescan started",
  "Auto rescan interval": "Auto rescan interval",
  "How often to automatically scan all drives for new media.": "How often to automatically scan all drives for new media.",
  "scan_interval_off": "Off"
```

- [ ] **Step 2: zh_CN.json 追加键**（同位置）：
```json
  "Rescan library": "重扫图库",
  "Scan all drives now and add new photos and videos to the library.": "立即扫描所有分区，将新增的照片和视频加入图库。",
  "Rescan now": "立即重扫",
  "Rescanning…": "重扫中…",
  "Library rescan started": "已开始重扫图库",
  "Auto rescan interval": "自动重扫间隔",
  "How often to automatically scan all drives for new media.": "每隔多久自动扫描所有分区以发现新媒体。",
  "scan_interval_off": "关闭"
```

- [ ] **Step 3: 校验 JSON**

Run: `node -e "JSON.parse(require('fs').readFileSync('src/assets/lang/en_US.json','utf8'));JSON.parse(require('fs').readFileSync('src/assets/lang/zh_CN.json','utf8'));console.log('OK')"`
Expected: `OK`

- [ ] **Step 4: 提交**
```bash
git add src/assets/lang/en_US.json src/assets/lang/zh_CN.json
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(i18n): Photos 重扫按钮与周期间隔文案（en/zh）"
```

---

### Task F4: PhotosSettings.vue —— 重扫按钮 + 间隔选择器（复用 .st-*，零新样式）

**Files:** Modify `src/views/Photos/PhotosSettings.vue`（template 在 Storage 卡片内加两行；script 加 data/methods/mounted；不新增任何 `<style>`）

- [ ] **Step 1: template —— 在 retention 行与"Thumbnail cache"行之间插入两行**

在 `</div>`（retention 行结束）之后、`<div class="st-row">`（Thumbnail cache 行开始）之前，插入：
```html
              <div class="st-row">
                <div class="st-row-text">
                  <div class="st-row-label">{{ $t('Rescan library') }}</div>
                  <div class="st-row-desc">{{ $t('Scan all drives now and add new photos and videos to the library.') }}</div>
                </div>
                <button class="st-btn" :disabled="scanBusy" @click="rescanNow">
                  <span v-if="scanBusy" class="st-spinner"></span>
                  {{ scanBusy ? $t('Rescanning…') : $t('Rescan now') }}
                </button>
              </div>

              <div class="st-row">
                <div class="st-row-text">
                  <div class="st-row-label">{{ $t('Auto rescan interval') }}</div>
                  <div class="st-row-desc">{{ $t('How often to automatically scan all drives for new media.') }}</div>
                </div>
                <div class="st-segmented">
                  <button v-for="opt in scanIntervalOptions" :key="opt.min"
                          :data-active="scanInterval === opt.min"
                          @click="setScanInterval(opt.min)">{{ opt.label }}</button>
                </div>
              </div>
```

- [ ] **Step 2: script —— data 加字段**

在 `data()` 的 `return { ... }` 里加：
```js
      scanBusy: false,
      scanInterval: this.$store.state.photos.scanIntervalMinutes || 1440,
```

- [ ] **Step 3: script —— computed 加档位（若无 computed 块则在 methods 前新增 computed）**

在 `computed` 中加（与现有 `prunableBytes` 同级）：
```js
    scanIntervalOptions() {
      return [
        { min: 0, label: this.$t('scan_interval_off') },
        { min: 360, label: '6h' },
        { min: 720, label: '12h' },
        { min: 1440, label: '24h' },
        { min: 10080, label: '7d' },
      ]
    },
```

- [ ] **Step 4: script —— methods 加 rescanNow / setScanInterval**

在 `methods` 中加（与 `clearCache` 同级）：
```js
    async rescanNow() {
      if (this.scanBusy) return
      this.scanBusy = true
      try {
        await photosService.triggerScan()
        this.showToast('check', this.$t('Library rescan started'))
      }
      catch (e) {
        this.showToast('trash', this.$t('Failed to start rebuild'))
      }
      finally {
        this.scanBusy = false
      }
    },
    async setScanInterval(min) {
      const prev = this.scanInterval
      this.scanInterval = min
      try {
        await this.$store.dispatch('photos/setScanInterval', min)
      }
      catch (e) {
        this.scanInterval = prev
        this.$buefy && this.$buefy.toast.open({ message: this.$t('Failed to save retention'), type: 'is-danger' })
      }
    },
```
（确认文件顶部已 import `photosService`——`clearCache`/`rebuildIndex` 已用 `photosService`，故已 import。）

- [ ] **Step 5: script —— mounted 读初值**

在 `mounted` 末尾（`fetchTrashRetention().then(...)` 之后）追加：
```js
    this.$store.dispatch('photos/fetchScanInterval').then(() => {
      this.scanInterval = this.$store.state.photos.scanIntervalMinutes || 1440
    })
```

- [ ] **Step 6: lint**

Run: `pnpm lint src/views/Photos/PhotosSettings.vue`
Expected: 无新增报错。

- [ ] **Step 7: 提交**
```bash
git add src/views/Photos/PhotosSettings.vue
git -c user.name='wiwiwilliam' -c user.email='yuwu0321@gmail.com' commit -m "feat(photos): 设置页加立即重扫按钮 + 自动重扫间隔选择器（复用 .st-*）"
```

---

### Task R: 全量回归

- [ ] **Step 1: 后端**（NimoOS-Photos）

Run: `cd /DATA/.nimoos-dev/NimoOS-Photos && go build ./... && go test ./... 2>&1 | tail -20`
Expected: 编译通过、测试全绿。

- [ ] **Step 2: 前端**（NimoOS-UI）

Run: `cd /DATA/.nimoos-dev/NimoOS-UI && pnpm exec vitest run 2>&1 | tail -5`
Expected: 全绿（含新增 photosScanInterval 用例）。

- [ ] **Step 3: 手动验收**

1. Photos 设置 → Storage：见"重扫图库"按钮与"自动重扫间隔"分段（关闭/6h/12h/24h/7d，默认高亮 24h）。
2. 点"立即重扫"→ 按钮转 busy、弹"已开始重扫"；RAID(`/media/RAID_*`)/U盘(`/mnt/Disk-*`)里的白名单文件进库。
3. 改间隔为 6h → 重开设置仍是 6h（持久化）；改"关闭"→ 周期扫停止。
4. `/DATA/AppData`、`/DATA/.system_data` 内文件不被索引；`.` 开头目录仍跳过。
5. 拔 U 盘 → 其照片仍在；插回 → 下次扫描恢复。
6. 设置页其余样式/布局无变化（确认零样式改动）。
