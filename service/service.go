package service

import (
	"database/sql"
	"path/filepath"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
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
}

// services is the unexported implementation of Services.
type services struct {
	db      *sql.DB
	indexer *Indexer
	watcher *Watcher
	albums  *AlbumService
	search  *SearchService
	faces   *FaceService
}

func (s *services) DB() *sql.DB           { return s.db }
func (s *services) Indexer() *Indexer     { return s.indexer }
func (s *services) Watcher() *Watcher     { return s.watcher }
func (s *services) Albums() *AlbumService { return s.albums }
func (s *services) Search() *SearchService { return s.search }
func (s *services) Faces() *FaceService   { return s.faces }

// NewService wires all service-layer components together from cfg and returns a
// ready-to-use Services handle. It panics if the database cannot be opened.
// A goroutine is started immediately to scan all WatchDirs and pair live photos.
func NewService(cfg *config.Config) Services {
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
	idx := NewIndexer(db, ml, thumbDir, cfg.Workers)
	watcher := NewWatcher(db, cfg.WatchDirs, idx, liveDir)
	albums := NewAlbumService(db)
	search := NewSearchService(db, ml)
	faces := NewFaceService(db)

	// 5. Kick off the initial directory scan in the background so startup is
	//    non-blocking. Live-photo pairing runs after all directories are queued.
	go func() {
		for _, dir := range cfg.WatchDirs {
			idx.ScanDirectory(dir) //nolint:errcheck
		}
		watcher.PairLivePhotos() //nolint:errcheck
	}()

	return &services{
		db:      db,
		indexer: idx,
		watcher: watcher,
		albums:  albums,
		search:  search,
		faces:   faces,
	}
}
