package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/model"
	"github.com/NimoTech/NimoOS-Common/utils/file"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/route"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/coreos/go-systemd/daemon"
	"go.uber.org/zap"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed build/sysroot/etc/nimoos/photos.conf.sample
	_confSample string
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	configFlag := flag.String("c", "", "config file path")
	versionFlag := flag.Bool("v", false, "version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.PhotosVersion)
		os.Exit(0)
	}

	fmt.Println("git commit:", commit)
	fmt.Println("build date:", date)

	if err := config.Init(*configFlag, _confSample); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize config: %v\n", err)
		os.Exit(1)
	}

	// Staging follows DataPath (see common.StagingDir comment); this reset
	// must happen before any consumer (route init, PruneStaging).
	common.StagingDir = filepath.Join(config.Cfg.DataPath, "tus-staging")

	// Create required data directories
	for _, dir := range []string{
		config.Cfg.DataPath,
		filepath.Join(config.Cfg.DataPath, "thumbs"),
		filepath.Join(config.Cfg.DataPath, "live"),
		filepath.Join(config.Cfg.DataPath, "ml-cache"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create directory %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	// The staging directory holds raw files from in-progress uploads, so
	// permissions are tightened to 0700 (matching the defensive mkdir in
	// route/v1/tus.go; MkdirAll does not change permissions on an existing
	// directory, so it must be created with 0700 here rather than folded
	// into the shared 0755 list above).
	if err := os.MkdirAll(common.StagingDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create staging directory %s: %v\n", common.StagingDir, err)
		os.Exit(1)
	}

	logger.LogInit(config.Cfg.LogPath, "nimoos-photos", "log")

	pub := service.NewMessageBusPublisher(ctx)
	svc := service.NewService(ctx, config.Cfg, pub)

	// Start background workers
	go svc.Watcher().Start(ctx)
	go svc.Indexer().Start(ctx)
	go svc.Faces().StartScheduler(ctx)
	go svc.Moments().StartScheduler(ctx)
	go svc.Embedder().Run(ctx)
	go svc.Rebuilder().MaybeAutoRebuild(svc.Indexer().MLReady)
	go svc.MountGuard().Run(ctx)

	// One-time backfill of video hover-preview sprites on startup (CAS guards
	// against re-entry): covers historical videos indexed before the upgrade
	// that never went through the new inline pre-generation path.
	go svc.Indexer().BackfillSprites(ctx)

	// One-time cleanup of orphaned TUS staging files on startup (scans both
	// the new and legacy directories; the legacy directory is only ever
	// swept once, on startup, as a fallback). Daily cleanup after that is
	// handled by the ticker below (new directory only).
	go func() {
		for _, dir := range []string{common.StagingDir, common.LegacyStagingDir} {
			if n, err := service.PruneStaging(dir, time.Duration(common.StagingMaxAge)*time.Hour); err != nil {
				zap.L().Warn("PruneStaging failed", zap.String("dir", dir), zap.Error(err))
			} else if n > 0 {
				zap.L().Info("PruneStaging removed orphans", zap.String("dir", dir), zap.Int("count", n))
			}
		}
	}()

	// Warm the filesystem-derived storage stats once at boot so the settings
	// page has cache/prunable numbers soon after startup instead of waiting
	// for the first request to kick the background walk.
	go svc.Storage().WarmFS()

	// Daily full cache cleanup: orphaned thumbnail directories + orphaned
	// face-thumbs + expired staging, same implementation as the manual
	// button on the settings page (POST /cache/prune).
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if res, err := svc.Storage().Prune(common.StagingDir, time.Duration(common.StagingMaxAge)*time.Hour); err != nil {
				zap.L().Warn("daily cache prune failed", zap.Error(err))
			} else if res.RemovedCount > 0 {
				zap.L().Info("daily cache prune", zap.Int("removed", res.RemovedCount), zap.Int64("freed_bytes", res.FreedBytes))
			}
			// PRAGMA optimize also runs once at startup (pkg/sqlite migrate()),
			// which is enough for a process that restarts periodically. But an
			// install that stays up for a long time (empty library at boot,
			// then a large import) never restarts, so sqlite_stat1 keeps
			// reflecting the empty-table snapshot from startup and the query
			// planner's cost estimates go stale as the library grows. Re-running
			// it here, on the same daily tick as the cache prune, is SQLite's
			// own recommended self-limiting pattern: it decides on its own
			// whether any table's stats are worth refreshing, so this is a
			// near-zero-cost no-op on a day nothing changed.
			if _, err := svc.DB().Exec(`PRAGMA optimize;`); err != nil {
				zap.L().Warn("daily PRAGMA optimize failed", zap.Error(err))
			}
		}
	}()

	// Bind to a random port on localhost
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic("failed to listen: " + err.Error())
	}

	// Write URL file for service discovery
	urlFilePath := filepath.Join(config.Cfg.RuntimePath, common.URLFileName)
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("failed to write URL file", zap.Error(err))
		// Non-fatal: Gateway registration uses the address directly.
	}

	// Register routes at Gateway
	gw, err := external.NewManagementService(config.Cfg.RuntimePath)
	if err != nil {
		panic("failed to connect to Gateway: " + err.Error())
	}
	for _, path := range []string{common.V1APIPath, common.V1DocPath, common.V1TUSPath} {
		if err := gw.CreateRoute(&model.Route{
			Path:   path,
			Target: "http://" + listener.Addr().String(),
		}); err != nil {
			panic("failed to register route " + path + ": " + err.Error())
		}
	}

	handler := route.InitRouter(ctx, svc, config.Cfg.RuntimePath, filepath.Join(config.Cfg.DataPath, "thumbs"))

	// Notify systemd
	if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Error("failed to notify systemd", zap.Error(err))
	} else if supported {
		logger.Info("notified systemd: ready")
	}

	logger.Info("NimoOS-Photos listening", zap.String("address", listener.Addr().String()))

	s := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := s.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", zap.Error(err))
	}
}
