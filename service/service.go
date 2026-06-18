package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
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
	rebuilder := NewRebuilder(parentCtx, db, idx, faces, taskReg, cfg.Workers)
	embedder := NewEmbedder(db, ml, idx, taskReg)

	// batch 上传完成后主动触发人脸聚类，让前端能看到从 0% 涨到 100% 的"识别人物" task。
	// faces.RunClustering 内部用 CAS 防重入，多个 batch 同时 done 也只会跑一次。
	idx.SetOnBatchDone(func() {
		go func() {
			if err := faces.RunClustering(parentCtx); err != nil {
				zap.L().Warn("post-batch face clustering failed", zap.Error(err))
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
	})

	// 5. Kick off the initial directory scan in the background so startup is
	//    non-blocking. ScanPending retries assets that failed in a prior run
	//    (e.g. unsupported format that now has a fallback decoder).
	go func() {
		idx.ScanPending()
		idx.pruneSystemMountAssets()
		pruneOrphanClipVectors(db) // sweep any vec0 rows left orphaned by past deletes
		idx.ScanAllRoots()
		watcher.PairLivePhotos() //nolint:errcheck
	}()

	// Real-time deletion subscriber: clean up indexed assets and CLIP vectors the
	// moment NimoOS reports a file/directory deletion via the MessageBus.
	// Periodic scans remain the durable safety net; this is a best-effort fast path.
	go StartMediaDeletedSubscriber(parentCtx, cfg.RuntimePath, idx)

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

func (zeroML) CLIPTextEmbed(string) ([]float32, error) { return make([]float32, 512), nil }
