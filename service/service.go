package service

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
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
	RestartWatcher(dirs []string)
}

// services is the unexported implementation of Services.
type services struct {
	db        *sql.DB
	indexer   *Indexer
	watcher   *Watcher
	albums    *AlbumService
	search    *SearchService
	faces     *FaceService
	tasks     *TaskRegistry
	embedder  *Embedder
	favorites *FavoritesService
	parentCtx context.Context
}

func (s *services) DB() *sql.DB            { return s.db }
func (s *services) Indexer() *Indexer      { return s.indexer }
func (s *services) Watcher() *Watcher      { return s.watcher }
func (s *services) Albums() *AlbumService  { return s.albums }
func (s *services) Search() *SearchService { return s.search }
func (s *services) Faces() *FaceService    { return s.faces }
func (s *services) Tasks() *TaskRegistry   { return s.tasks }
func (s *services) Embedder() *Embedder          { return s.embedder }
func (s *services) Favorites() *FavoritesService { return s.favorites }

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
	favorites := NewFavoritesService(db)
	search := NewSearchService(db, ml)
	faces := NewFaceService(db)
	faces.SetTaskRegistry(taskReg)
	embedder := NewEmbedder(db, ml, idx, taskReg)

	// batch 上传完成后主动触发人脸聚类，让前端能看到从 0% 涨到 100% 的"识别人物" task。
	// faces.RunClustering 内部用 CAS 防重入，多个 batch 同时 done 也只会跑一次。
	idx.SetOnBatchDone(func() {
		go func() {
			if err := faces.RunClustering(parentCtx); err != nil {
				zap.L().Warn("post-batch face clustering failed", zap.Error(err))
			}
		}()
	})

	// 5. Kick off the initial directory scan in the background so startup is
	//    non-blocking. ScanPending retries assets that failed in a prior run
	//    (e.g. unsupported format that now has a fallback decoder).
	go func() {
		idx.ScanPending()
		for _, dir := range cfg.WatchDirs {
			idx.ScanDirectory(dir) //nolint:errcheck
		}
		watcher.PairLivePhotos() //nolint:errcheck
	}()

	return &services{
		db:        db,
		indexer:   idx,
		watcher:   watcher,
		albums:    albums,
		search:    search,
		faces:     faces,
		tasks:     taskReg,
		embedder:  embedder,
		favorites: favorites,
		parentCtx: parentCtx,
	}
}

func (s *services) RestartWatcher(dirs []string) {
	s.watcher.Restart(s.parentCtx, dirs)
}
