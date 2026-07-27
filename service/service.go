package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/aesthetic"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"go.uber.org/zap"
)

// Services is the top-level dependency container used by the HTTP layer and
// background workers. All methods are safe to call concurrently.
type Services interface {
	DB() *sql.DB
	Indexer() *Indexer
	Watcher() *Watcher
	Albums() *AlbumService
	Search() *SearchService
	Faces() *FaceService
	Tasks() *TaskRegistry
	Embedder() *Embedder
	Favorites() *FavoritesService
	Trash() *TrashService
	Views() *ViewsService
	Persons() *PersonService
	Geo() *GeoService
	Places() *PlacesService
	SmartViews() *SmartViewService
	Storage() *StorageService
	Rebuilder() *Rebuilder
	MountGuard() *MountGuard
	RestartWatcher(dirs []string)
	RestartScanTicker(minutes int)
}

// services is the unexported implementation of Services.
type services struct {
	db               *sql.DB
	indexer          *Indexer
	watcher          *Watcher
	albums           *AlbumService
	search           *SearchService
	faces            *FaceService
	tasks            *TaskRegistry
	embedder         *Embedder
	favorites        *FavoritesService
	trash            *TrashService
	views            *ViewsService
	persons          *PersonService
	geo              *GeoService
	places           *PlacesService
	smartViews       *SmartViewService
	storage          *StorageService
	rebuilder        *Rebuilder
	mountGuard       *MountGuard
	parentCtx        context.Context
	scanMu           sync.Mutex
	scanTickerCancel context.CancelFunc
}

func (s *services) DB() *sql.DB                   { return s.db }
func (s *services) Indexer() *Indexer             { return s.indexer }
func (s *services) Watcher() *Watcher             { return s.watcher }
func (s *services) Albums() *AlbumService         { return s.albums }
func (s *services) Search() *SearchService        { return s.search }
func (s *services) Faces() *FaceService           { return s.faces }
func (s *services) Tasks() *TaskRegistry          { return s.tasks }
func (s *services) Embedder() *Embedder           { return s.embedder }
func (s *services) Favorites() *FavoritesService  { return s.favorites }
func (s *services) Trash() *TrashService          { return s.trash }
func (s *services) Views() *ViewsService          { return s.views }
func (s *services) Persons() *PersonService       { return s.persons }
func (s *services) Geo() *GeoService              { return s.geo }
func (s *services) Places() *PlacesService        { return s.places }
func (s *services) SmartViews() *SmartViewService { return s.smartViews }
func (s *services) Storage() *StorageService      { return s.storage }
func (s *services) Rebuilder() *Rebuilder         { return s.rebuilder }
func (s *services) MountGuard() *MountGuard       { return s.mountGuard }

// NewService wires all service-layer components together from cfg and returns a
// ready-to-use Services handle. It panics if the database cannot be opened.
// A goroutine is started immediately to scan all WatchDirs and pair live photos.
func NewService(parentCtx context.Context, cfg *config.Config, pub TaskPublisher) Services {
	// 1. Open (or create) the SQLite database.
	dbPath := filepath.Join(cfg.DataPath, "photos.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		panic("nimoos-photos: failed to open database: " + err.Error())
	}

	// 2. Build the ML client.
	ml := mlclient.New(cfg.MLEndpoint)

	// 3. Derived data directories.
	thumbDir := filepath.Join(cfg.DataPath, "thumbs")
	liveDir := filepath.Join(cfg.DataPath, "live")

	// 4. Assemble individual services.
	taskReg := NewTaskRegistry(pub)
	// A:停滞清扫器兜底——任何 running 任务长时间无更新即强制收尾,杜绝永久僵尸任务
	//（尤其覆盖人脸聚类这类没有 DB 真值、前端无法对账的任务)。
	go taskReg.StartStaleSweeper(parentCtx, taskStaleTimeout, taskSweepInterval)
	idx := NewIndexer(db, ml, thumbDir, cfg.Workers)
	idx.SetTaskRegistry(taskReg)
	watcher := NewWatcher(db, cfg.WatchDirs, idx, liveDir)
	albums := NewAlbumService(db)
	// Wire post-index album assignment: uploads carrying an albumId join the
	// album as soon as their asset record exists. AddAsset is idempotent
	// (album_assets uses INSERT OR IGNORE).
	idx.SetAlbumAssigner(func(assetID, albumID string) {
		if err := albums.AddAsset(albumID, assetID); err != nil {
			zap.L().Warn("post-index album assign failed",
				zap.String("album_id", albumID), zap.String("asset_id", assetID), zap.Error(err))
		}
	})
	favorites := NewFavoritesService(db)
	trash := NewTrashService(db, "/DATA/Gallery", thumbDir)
	views := NewViewsService(db)
	search := NewSearchService(db, ml)
	smartViews := NewSmartViewService(db, search)
	persons := NewPersonService(db)
	gaz, gerr := geo.Load()
	if gerr != nil {
		panic("nimoos-photos: failed to load gazetteer: " + gerr.Error())
	}
	geoSvc := NewGeoService(db, gaz)
	placesSvc := NewPlacesServiceWithAlbums(db, gaz, geoSvc, albums)
	faces := NewFaceService(db)
	faces.SetTaskRegistry(taskReg)
	faces.SetIndexIdleSource(idx.IdleFor) // 安全网聚类去抖:索引活动安静够久才触发
	faces.SetML(ml)                       // RunPipeline 检测阶段用
	faces.SetThumbDir(thumbDir)           // RunPipeline 视频关键帧缩略图用
	rebuilder := NewRebuilder(parentCtx, db, idx, faces, taskReg, cfg.Workers)
	embedder := NewEmbedder(db, ml, idx, taskReg)
	// ML 恢复链尾补跑人脸检测(覆盖掉线期间的检测欠账)：函数字段注入，避免
	// Embedder 直接依赖 FaceService 类型(同 MountGuard 的注入模式)。
	embedder.SetOnRecovered(func(ctx context.Context) {
		if err := faces.RunPipeline(ctx); err != nil {
			zap.L().Warn("post-recovery face pipeline failed", zap.Error(err))
		}
	})
	// 美学评分头:加载失败只告警降级(功能整体不可用,分数留 NULL)。
	if cfg.AestheticEnabled {
		if head, err := aesthetic.Load(); err != nil {
			zap.L().Warn("aesthetic: 内嵌头加载失败,评分功能停用", zap.Error(err))
		} else {
			idx.SetAestheticHead(head)
			embedder.SetAestheticHead(head)
			if err := EnsureAestheticHeadVer(db, head.Version()); err != nil {
				zap.L().Warn("aesthetic: 头版本对齐失败", zap.Error(err))
			}
			// 启动即补扫:纯本地计算不等 ML 就绪(与 OCR 的关键差异)。
			go func() {
				if err := embedder.BackfillAesthetic(parentCtx); err != nil {
					zap.L().Warn("aesthetic: 启动补扫失败", zap.Error(err))
				}
			}()
		}
	}

	// MountGuard: 追踪 /media/* 可移动盘的挂载/拔出,维护 assets.offline。
	// 回调用函数字段注入以避免与 Watcher/Indexer/Embedder 产生导入依赖:
	//   - watcherRestart 直接闭包捕获 watcher/parentCtx/cfg，重新 Add 配置中
	//     watch 的目录(对没被配置监听的目录是无副作用的 no-op)；
	//   - scanDir 复用 Indexer.ScanDirectoryOnce 自愈新增/删除的文件，并与
	//     watcher 挂载轮询共享同一份 per-root 去重，避免同一挂载被重复补扫；
	//   - backfill/backfillOCR 复用 Embedder，修复换代次重建期间 offline
	//     资产恢复后缺失的 CLIP/OCR。
	mountGuard := NewMountGuard(db)
	mountGuard.SetWatcherRestart(func() { watcher.Restart(parentCtx, cfg.WatchDirs) })
	mountGuard.SetScanDir(func(mount string) error {
		_, err := idx.ScanDirectoryOnce(mount)
		return err
	})
	mountGuard.SetBackfill(embedder.Backfill)
	mountGuard.SetBackfillOCR(embedder.BackfillOCR)
	// Run() 由 main.go 与其它后台 worker 一起以 goroutine 启动。

	// CaptionFeeder：把已索引资产旁路投喂给 NimoOS-Parser 生成 caption
	// （照片知识库子项目二)。Parser 未部署（discoveryFile 不存在）时
	// parserclient 返回 ErrParserUnavailable，全链路静默跳过。
	// parserClient 与下方 Puller 共用同一份 discoveryFile/http.Client。
	parserClient := parserclient.New(cfg.RuntimePath)
	feeder := NewCaptionFeeder(db, parserClient, thumbDir)
	idx.SetOnIndexed(func(id string) { feeder.FeedOne(parentCtx, id) })
	// 删除/回收站全路径联动（Task 4）：软删/物理删（含清空回收站、Indexer 硬删、
	// SearchService 硬删）异步通知 Parser 删 caption；恢复后置 caption_synced=0
	// 待下轮补扫重投。函数字段注入，各 service 无需 import CaptionFeeder 类型。
	idx.SetCaptionDelete(feeder.DeleteRemote)
	search.SetCaptionDelete(feeder.DeleteRemote)
	trash.SetCaptionDelete(feeder.DeleteRemote)
	trash.SetCaptionRestore(feeder.OnRestore)
	// 启动即补扫一次，捡起服务重启前遗留的欠投喂资产。
	go func() {
		if err := feeder.Backfill(parentCtx); err != nil {
			zap.L().Warn("caption: 启动补扫失败", zap.Error(err))
		}
	}()

	// Puller：周期性从 Parser 拉取已生成的 caption 回流进本地 asset_caption
	// 表（照片知识库子项目二回流侧，消费/检索是后续子项目）。挂点节奏照抄
	// CaptionFeeder 同款：启动即拉一次 + 挂在 SetOnBatchDone 链尾跟随批次
	// 节奏。lister 出错（含 Parser 未部署）不向上传播，ErrParserUnavailable
	// 完全静默、其它错误仅 Warn 留痕，均不影响索引主流程。
	puller := NewPuller(db, parserClient)
	go func() {
		if _, err := puller.PullOnce(parentCtx); err != nil && !errors.Is(err, parserclient.ErrParserUnavailable) {
			zap.L().Warn("caption pull: 启动拉取失败", zap.Error(err))
		}
	}()

	// batch 上传完成后触发人脸检测+聚类一体任务，让前端能看到从 0% 涨到 100% 的
	// "识别人物" task（真实进度，而非旧的聚类专属假进度）。
	// faces.RunPipeline 内部用 CAS 防重入，多个 batch 同时 done 也只会跑一次。
	idx.SetOnBatchDone(func() {
		go func() {
			if err := faces.RunPipeline(parentCtx); err != nil {
				zap.L().Warn("post-batch face pipeline failed", zap.Error(err))
			}
		}()
		// CLIP/OCR 兜底补跑:索引期间 ML 冷加载/worker 回收会让 embedClip/ocrAsset
		// 偶发失败且被吞(processFile 不因 ML 失败拒绝入库),而 Embedder 的恢复链
		// 只在 ML「掉线→恢复」跳变时触发——ML 全程在线就永远没人补,资产无限期
		// 缺向量、语义搜索搜不到(真实故障:两张鱼图撞上模型冷加载窗口)。批次末尾
		// 补一手,CAS+rerunPending 已防重入,无欠账时两个调用都是秒级空跑。
		go func() {
			if err := embedder.Backfill(parentCtx); err != nil {
				zap.L().Warn("post-batch clip backfill failed", zap.Error(err))
			}
			if err := embedder.BackfillOCR(parentCtx); err != nil {
				zap.L().Warn("post-batch ocr backfill failed", zap.Error(err))
			}
			if err := embedder.BackfillDocVerdicts(parentCtx); err != nil {
				zap.L().Warn("post-batch doc verdict backfill failed", zap.Error(err))
			}
			if err := embedder.BackfillAesthetic(parentCtx); err != nil {
				zap.L().Warn("post-batch aesthetic backfill failed", zap.Error(err))
			}
			// caption 补扫链尾:批次末尾捡起漏投喂的资产(Parser 未部署时静默空跑)。
			if err := feeder.Backfill(parentCtx); err != nil {
				zap.L().Warn("post-batch caption backfill failed", zap.Error(err))
			}
			// caption 拉取链尾:批次末尾从 Parser 拉取新增/更新的 caption 回填
			// 本地表(Parser 未部署时静默跳过,一般失败仅 Warn 留痕)。
			if _, err := puller.PullOnce(parentCtx); err != nil && !errors.Is(err, parserclient.ErrParserUnavailable) {
				zap.L().Warn("post-batch caption pull failed", zap.Error(err))
			}
		}()
		go func() {
			for {
				n, err := geoSvc.BackfillPending(500)
				if err != nil || n == 0 {
					return
				}
			}
		}()
		go func() {
			if err := smartViews.EvaluateAllLive(); err != nil {
				zap.L().Warn("smart view incremental evaluate failed", zap.Error(err))
			}
		}()
		// 批量导入视频后补漏 + 借任务栏展示转码进度;CAS 防与启动补跑/多批次并发重入。
		// (内联预生成已在索引时逐条排队,批次末尾多数已就绪,该轮只处理剩余欠账,
		// total 反映真实剩余量——正是任务栏该显示的。)
		go func() {
			idx.BackfillSprites(parentCtx)
		}()
	})

	// 5. Kick off the initial directory scan in the background so startup is
	//    non-blocking. ScanPending retries assets that failed in a prior run
	//    (e.g. unsupported format that now has a fallback decoder).
	go func() {
		idx.ScanPending()
		idx.pruneSystemMountAssets()
		idx.pruneRcloneMountAssets(enumerateRcloneMounts())
		idx.pruneSnapshotAssets()  // 清掉误入库的 btrfs .snapshots 快照子卷资产
		pruneOrphanClipVectors(db) // sweep any vec0 rows left orphaned by past deletes
		pruneVideoOCR(db)          // 视频不再做 OCR:清掉历史遗留的视频 OCR 行,使其退出「OCR/文档」分类
		idx.ScanAllRoots()
		watcher.PairLivePhotos() //nolint:errcheck
	}()

	// Real-time deletion subscriber: clean up indexed assets and CLIP vectors the
	// moment NimoOS reports a file/directory deletion via the MessageBus.
	// Periodic scans remain the durable safety net; this is a best-effort fast path.
	go StartMediaDeletedSubscriber(parentCtx, cfg.RuntimePath, idx)

	// Real-time creation subscriber: index newly landed files (upload/copy/move)
	// the moment NimoOS reports them. 独立连接:主服务未升级时自退避,不连累 deleted。
	go StartMediaCreatedSubscriber(parentCtx, cfg.RuntimePath, idx)

	// ML 后端自愈：worker 卡死（端口在听但 /ping 不应答）时自动 docker restart
	mlWatchdog := NewMLWatchdog(ml.IsReady, dockerRunner{})
	go mlWatchdog.Run(parentCtx)

	// 启动时重评估一次 live Smart View：Evaluate 会从 conds_raw 现解析并重新打分，
	// 解析器 / 分数标定升级后旧视图无需用户干预即可自愈。semantic 条件需要 ML
	// 文本向量，所以最多等 2 分钟 ML 就绪；失败只告警，下个 batch 会再触发。
	go func() {
		for i := 0; i < 24 && !ml.IsReady(); i++ {
			select {
			case <-parentCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if err := smartViews.EvaluateAllLive(); err != nil {
			zap.L().Warn("startup smart view evaluate failed", zap.Error(err))
		}
	}()

	geoSvc.StartScheduler(parentCtx)

	// Storage stats for the settings page. statfs anchors on the first watch
	// dir (the library volume); falls back to /DATA.
	statfsDir := "/DATA"
	if len(cfg.WatchDirs) > 0 {
		statfsDir = cfg.WatchDirs[0]
	}
	faceThumbDir := filepath.Join(cfg.DataPath, "face-thumbs")
	storageSvc := NewStorageService(db, dbPath, thumbDir, faceThumbDir, statfsDir)

	// 回收站自动清理：启动时跑一次，之后每 24 小时跑一次，到期项永久删除。
	// CLIP 向量孤儿清理也在同一个每日 ticker 内独立运行，与回收站 purge 解耦。
	go func() {
		runPurge := func() {
			days := 30
			if config.Cfg != nil && config.Cfg.RetentionDays > 0 {
				days = config.Cfg.RetentionDays
			}
			if err := trash.PurgeExpired(days); err != nil {
				zap.L().Warn("trash auto-purge failed", zap.Error(err))
			}
		}
		runPurge()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-parentCtx.Done():
				return
			case <-ticker.C:
				runPurge()
				// Daily safety net: drop any vec0 rows whose parent asset was deleted
				// since the last startup or purge sweep.
				pruneOrphanClipVectors(db)
			}
		}
	}()

	svc := &services{
		db:         db,
		indexer:    idx,
		watcher:    watcher,
		albums:     albums,
		search:     search,
		faces:      faces,
		tasks:      taskReg,
		embedder:   embedder,
		favorites:  favorites,
		trash:      trash,
		views:      views,
		persons:    persons,
		geo:        geoSvc,
		places:     placesSvc,
		smartViews: smartViews,
		storage:    storageSvc,
		rebuilder:  rebuilder,
		mountGuard: mountGuard,
		parentCtx:  parentCtx,
	}
	svc.RestartScanTicker(cfg.ScanInterval)
	return svc
}

func (s *services) RestartWatcher(dirs []string) {
	if s.watcher == nil {
		return // NewTestServices 不接 watcher；handler 测试走到这里直接跳过
	}
	s.watcher.Restart(s.parentCtx, dirs)
}

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
				s.indexer.ScanAllRoots()
			}
		}
	}()
}

// NewTestServices builds a minimal Services backed by db, for handler tests.
// Only SmartViews, Albums, and Search are wired; other accessors return nil.
func NewTestServices(db *sql.DB) Services {
	search := NewSearchService(db, zeroML{})
	albums := NewAlbumService(db)
	return &services{
		db:         db,
		search:     search,
		albums:     albums,
		smartViews: NewSmartViewService(db, search),
		storage:    NewStorageService(db, "", os.TempDir(), os.TempDir(), os.TempDir()),
	}
}

// zeroML satisfies the textEmbedder interface with zero-value CLIP embeddings.
// It is used only in tests where actual ML inference is not needed.
type zeroML struct{}

func (zeroML) CLIPTextEmbed(string) ([]float32, error) { return make([]float32, common.CLIPDim), nil }
