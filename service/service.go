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
	"github.com/NimoTech/NimoOS-Photos/pkg/aiclient"
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
	Moments() *MomentsService
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
	moments          *MomentsService
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
func (s *services) Moments() *MomentsService      { return s.moments }
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
	// Wire the threshold self-calibration resolver to the live DB so the
	// five accessors below (assignAutoDist/assignSuggestDist/momentGap/
	// tightEps/mergeEps) can read calibration_state and the stored
	// calibration profile, not just conf/code defaults.
	SetCalibrationDB(db)

	// 2. Build the ML client.
	ml := mlclient.New(cfg.MLEndpoint)

	// 3. Derived data directories.
	thumbDir := filepath.Join(cfg.DataPath, "thumbs")
	liveDir := filepath.Join(cfg.DataPath, "live")

	// 4. Assemble individual services.
	taskReg := NewTaskRegistry(pub)
	// A: stale-sweeper backstop — any "running" task with no update for too
	// long is force-finished, ruling out permanent zombie tasks (especially
	// covers tasks like face clustering that have no DB ground truth for the
	// frontend to reconcile against).
	go taskReg.StartStaleSweeper(parentCtx, taskStaleTimeout, taskSweepInterval)
	idx := NewIndexer(db, ml, thumbDir, cfg.Workers)
	idx.SetTaskRegistry(taskReg)
	idx.SetPreviewPregen(cfg.PreviewPregen)
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

	// Smart Moments: trip/theme engine scheduling assembly + LLM naming (Task
	// 4). Recipe hot-reload goes through PUT /v1/photos/moments/recipes (Task
	// 5, not yet wired to a route); here we just seed the built-in recipes
	// and assemble the scheduling layer. aiClient goes through NimoOS-AI's
	// `_internal` direct connection (localhost-only, JWT-exempt); when the AI
	// service isn't deployed, aiURLFile can't be read, and LLM naming is
	// silently skipped end-to-end (best-effort, doesn't affect the
	// template-backed title).
	momentStore := NewMomentStore(db)
	if err := momentStore.SeedDefaultRecipes(); err != nil {
		zap.L().Warn("moments: failed to seed built-in recipes", zap.Error(err))
	}
	aiClient := aiclient.New(filepath.Join(cfg.RuntimePath, "ai.url"))
	momentsSvc := NewMomentsService(db, momentStore, search, RealClipVecLoader(db), aiClient)
	momentsSvc.SetTaskRegistry(taskReg)
	momentsSvc.SetLoadCover(RealCoverImageLoader(thumbDir))
	// Run a catch-up pass immediately on startup, picking up any recompute
	// debt left over from before the service restarted (newly-installed
	// recipes, a half-finished recompute before the last restart, etc.);
	// after that it follows the batch cadence (see the SetOnBatchDone tail
	// below) and the daily schedule (StartScheduler in main.go).
	go func() {
		if err := momentsSvc.RecomputeAll(parentCtx); err != nil {
			zap.L().Warn("moments: startup recompute failed", zap.Error(err))
		}
	}()
	gaz, gerr := geo.Load()
	if gerr != nil {
		panic("nimoos-photos: failed to load gazetteer: " + gerr.Error())
	}
	geoSvc := NewGeoService(db, gaz)
	placesSvc := NewPlacesServiceWithAlbums(db, gaz, geoSvc, albums)
	faces := NewFaceService(db)
	faces.SetTaskRegistry(taskReg)
	faces.SetIndexIdleSource(idx.IdleFor)       // safety-net clustering debounce: only triggers once index activity has been quiet long enough
	faces.SetML(ml)                             // used by RunPipeline's detection stage
	faces.SetThumbDir(thumbDir)                 // used by RunPipeline for video keyframe thumbnails
	faces.SetMarkerDir(cfg.DataPath)            // one-shot migration markers (e.g. exemplar-assignment, see exemplar_migrate.go)
	faces.SetDuePurger(persons.PurgeDuePersons) // sweeps hidden persons past their undo grace period
	rebuilder := NewRebuilder(parentCtx, db, idx, faces, taskReg, cfg.Workers)
	embedder := NewEmbedder(db, ml, idx, taskReg)
	// One shared gate throttles the heavy backfill chains; faces/geo/smart
	// views/moments stay ungated (cheap or already self-limiting). Chain
	// names are deliberately distinct — see backfillGate's doc comment.
	backfillGateShared := newBackfillGate(defaultBackfillGateInterval)
	embedder.SetGate(backfillGateShared)
	// ML-recovery tail catches up on face detection (covers detection debt
	// accrued while offline): injected as a function field to avoid Embedder
	// depending directly on the FaceService type (same injection pattern as MountGuard).
	embedder.SetOnRecovered(func(ctx context.Context) {
		if err := faces.RunPipeline(ctx); err != nil {
			zap.L().Warn("post-recovery face pipeline failed", zap.Error(err))
		}
	})
	// Aesthetic scoring head: on load failure, just warn and degrade (the
	// feature is entirely unavailable, scores stay NULL).
	if cfg.AestheticEnabled {
		if head, err := aesthetic.Load(); err != nil {
			zap.L().Warn("aesthetic: embedded head failed to load, scoring disabled", zap.Error(err))
		} else {
			idx.SetAestheticHead(head)
			embedder.SetAestheticHead(head)
			if err := EnsureAestheticHeadVer(db, head.Version()); err != nil {
				zap.L().Warn("aesthetic: head version alignment failed", zap.Error(err))
			}
			// Sweep on startup: purely local computation, doesn't wait for ML readiness (the key difference from OCR).
			go func() {
				if err := embedder.BackfillAesthetic(parentCtx); err != nil {
					zap.L().Warn("aesthetic: startup backfill sweep failed", zap.Error(err))
				}
			}()
		}
	}

	// MountGuard: tracks mount/unmount of removable /media/* drives,
	// maintaining assets.offline. Callbacks are injected as function fields
	// to avoid an import dependency on Watcher/Indexer/Embedder:
	//   - watcherRestart directly closes over watcher/parentCtx/cfg, re-Adding
	//     the configured watch directories (a no-op with no side effects for
	//     directories not configured to be watched);
	//   - scanDir reuses Indexer.ScanDirectoryOnce to self-heal added/removed
	//     files, sharing the same per-root dedup as the watcher's mount
	//     polling, avoiding a redundant sweep of the same mount;
	//   - backfill/backfillOCR reuse Embedder to repair CLIP/OCR missing after
	//     an offline asset recovers during a rebuild across versions.
	mountGuard := NewMountGuard(db)
	mountGuard.SetWatcherRestart(func() { watcher.Restart(parentCtx, cfg.WatchDirs) })
	mountGuard.SetScanDir(func(mount string) error {
		_, err := idx.ScanDirectoryOnce(mount)
		return err
	})
	mountGuard.SetBackfill(embedder.Backfill)
	mountGuard.SetBackfillOCR(embedder.BackfillOCR)
	// Run() is started as a goroutine in main.go alongside the other background workers.

	// CaptionFeeder: side-feeds already-indexed assets to NimoOS-Parser to
	// generate captions (photo knowledge base sub-project two). When Parser
	// isn't deployed (discoveryFile doesn't exist), parserclient returns
	// ErrParserUnavailable and the whole chain is silently skipped.
	// parserClient shares the same discoveryFile/http.Client with the Puller below.
	parserClient := parserclient.New(cfg.RuntimePath)
	feeder := NewCaptionFeeder(db, parserClient, thumbDir)
	idx.SetOnIndexed(func(id string) { feeder.FeedOne(parentCtx, id) })
	// Full delete/trash-path cascade (Task 4): soft delete/hard delete
	// (including emptying the trash, Indexer's hard delete, SearchService's
	// hard delete) asynchronously notifies Parser to delete the caption;
	// after a restore, caption_synced=0 is set, to be re-fed on the next
	// backfill sweep. Injected as a function field so services don't need to
	// import the CaptionFeeder type.
	idx.SetCaptionDelete(feeder.DeleteRemote)
	search.SetCaptionDelete(feeder.DeleteRemote)
	trash.SetCaptionDelete(feeder.DeleteRemote)
	trash.SetCaptionRestore(feeder.OnRestore)
	// Sweep once on startup, catching up on assets left un-fed before the service restarted.
	go func() {
		if err := feeder.Backfill(parentCtx); err != nil {
			zap.L().Warn("caption: startup backfill sweep failed", zap.Error(err))
		}
	}()

	// Puller: periodically pulls generated captions from Parser back into the
	// local asset_caption table (the return-flow side of photo knowledge base
	// sub-project two; consumption/retrieval is a later sub-project). Its
	// cadence mirrors CaptionFeeder's: pull once on startup + hooked onto the
	// SetOnBatchDone tail to follow the batch cadence. A lister error
	// (including Parser not deployed) doesn't propagate upward:
	// ErrParserUnavailable is fully silent, other errors are just Warn-logged
	// — none of it affects the main indexing flow. The deleter reuses the
	// same parserClient: on a genuine orphan (no such id in local assets), it
	// best-effort deletes the vector back on Parser's side, reconciling for
	// cases where a fire-and-forget delete notification was lost.
	puller := NewPuller(db, parserClient, parserClient)
	go func() {
		if _, err := puller.PullOnce(parentCtx); err != nil && !errors.Is(err, parserclient.ErrParserUnavailable) {
			zap.L().Warn("caption pull: startup pull failed", zap.Error(err))
		}
	}()

	// After a batch upload completes, trigger the combined face
	// detection+clustering task, so the frontend can see the "Recognizing
	// people" task climb from 0% to 100% (real progress, rather than the old
	// clustering-only fake progress).
	// faces.RunPipeline uses CAS internally to prevent re-entrancy, so it only runs once even if multiple batches finish at the same time.
	idx.SetOnBatchDone(func() {
		go func() {
			if err := faces.RunPipeline(parentCtx); err != nil {
				zap.L().Warn("post-batch face pipeline failed", zap.Error(err))
			}
		}()
		// CLIP/OCR backstop catch-up: during indexing, ML cold-loading/worker
		// reclamation can cause embedClip/ocrAsset to occasionally fail and
		// get swallowed (processFile doesn't reject ingestion just because ML
		// failed), while Embedder's recovery chain only triggers on an ML
		// "offline→recovered" transition — if ML stays online the whole
		// time, nobody ever catches up, leaving assets permanently missing
		// vectors and unsearchable by semantic search (real incident: two
		// fish photos hit a model cold-load window). Catch up at the end of
		// each batch; CAS+rerunPending already prevent re-entrancy, so both
		// calls are a fast no-op when there's no debt.
		go backfillGateShared.Run("post-batch-backfill", func() {
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
			// Caption backfill tail: catches up on any assets missed during
			// feeding at the end of a batch (a silent no-op when Parser isn't deployed).
			if err := feeder.Backfill(parentCtx); err != nil {
				zap.L().Warn("post-batch caption backfill failed", zap.Error(err))
			}
			// Caption pull tail: at the end of a batch, pulls new/updated
			// captions from Parser back into the local table (silently
			// skipped when Parser isn't deployed; other failures are just Warn-logged).
			if _, err := puller.PullOnce(parentCtx); err != nil && !errors.Is(err, parserclient.ErrParserUnavailable) {
				zap.L().Warn("post-batch caption pull failed", zap.Error(err))
			}
		})
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
		// Catches up after bulk video imports + uses the task bar to show
		// transcode progress; CAS prevents re-entrancy against startup
		// catch-up/concurrent batches. (Inline pregeneration is already
		// queued per-item during indexing, so most are ready by the end of
		// the batch — this round only handles the remaining debt, and total
		// reflects the true remaining count, exactly what the task bar should show.)
		go backfillGateShared.Run("sprite-backfill", func() {
			idx.BackfillSprites(parentCtx)
		})
		// Smart Moments tail: recompute moment at the end of a batch (trip
		// time-window segmentation + theme CLIP/caption hits change as new
		// assets arrive); CAS prevents re-entrancy against startup catch-up/
		// daily scheduling; LLM naming is best-effort — a failure is just
		// Warn-logged and doesn't affect the other tail steps.
		go func() {
			if err := momentsSvc.RecomputeAll(parentCtx); err != nil {
				zap.L().Warn("post-batch moments recompute failed", zap.Error(err))
			}
		}()
	})

	// 5. Kick off the initial directory scan in the background so startup is
	//    non-blocking. ScanPending retries assets that failed in a prior run
	//    (e.g. unsupported format that now has a fallback decoder).
	go func() {
		idx.ScanPending()
		idx.pruneSystemMountAssets()
		idx.pruneRcloneMountAssets(enumerateRcloneMounts())
		idx.pruneSnapshotAssets()  // clears out assets wrongly ingested from btrfs .snapshots snapshot subvolumes
		pruneOrphanClipVectors(db) // sweep any vec0 rows left orphaned by past deletes
		pruneVideoOCR(db)          // videos no longer get OCR: clears legacy video OCR rows so they drop out of the "OCR/documents" category
		idx.ScanAllRoots()
		watcher.PairLivePhotos() //nolint:errcheck
	}()

	// Real-time deletion subscriber: clean up indexed assets and CLIP vectors the
	// moment NimoOS reports a file/directory deletion via the MessageBus.
	// Periodic scans remain the durable safety net; this is a best-effort fast path.
	go StartMediaDeletedSubscriber(parentCtx, cfg.RuntimePath, idx)

	// Real-time creation subscriber: index newly landed files (upload/copy/move)
	// the moment NimoOS reports them. Separate connection: self-backs-off when the main service hasn't been upgraded, without dragging down `deleted`.
	go StartMediaCreatedSubscriber(parentCtx, cfg.RuntimePath, idx)

	// ML backend self-heal: automatically docker restart when the worker hangs (port listening but /ping doesn't respond)
	mlWatchdog := NewMLWatchdog(ml.IsReady, dockerRunner{})
	go mlWatchdog.Run(parentCtx)

	// Re-evaluate live Smart Views once on startup: Evaluate re-parses from
	// conds_raw and rescoring on the fly, so old views self-heal without
	// user intervention after a parser/score-calibration upgrade. Semantic
	// conditions need an ML text vector, so wait up to 2 minutes for ML
	// readiness; a failure is just a warning, and the next batch will trigger it again.
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

	// Trash auto-cleanup: runs once on startup, then every 24 hours; expired
	// entries are permanently deleted. CLIP vector orphan cleanup also runs
	// independently within the same daily ticker, decoupled from the trash purge.
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
		moments:    momentsSvc,
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
		return // NewTestServices doesn't wire up a watcher; handler tests skip straight through here
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
// Only SmartViews, Albums, Search, Persons, and Faces are wired; other
// accessors return nil. Faces is a bare NewFaceService(db) (no TaskRegistry/
// ML/marker dir injected) -- enough for the calibration status/history/
// profile endpoints (route/v1/persons_calibration_test.go), which only touch
// s.db, never RunClustering's fuller dependency set.
func NewTestServices(db *sql.DB) Services {
	search := NewSearchService(db, zeroML{})
	albums := NewAlbumService(db)
	return &services{
		db:         db,
		search:     search,
		albums:     albums,
		faces:      NewFaceService(db),
		persons:    NewPersonService(db),
		smartViews: NewSmartViewService(db, search),
		storage:    NewStorageService(db, "", os.TempDir(), os.TempDir(), os.TempDir()),
	}
}

// zeroML satisfies the textEmbedder interface with zero-value CLIP embeddings.
// It is used only in tests where actual ML inference is not needed.
type zeroML struct{}

func (zeroML) CLIPTextEmbed(string) ([]float32, error) { return make([]float32, common.CLIPDim), nil }
