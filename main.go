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

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/route"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/NimoTech/NimoOS-Common/model"
	"github.com/NimoTech/NimoOS-Common/utils/file"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
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

	logger.LogInit(config.Cfg.LogPath, "nimoos-photos", "log")

	pub := service.NewMessageBusPublisher(ctx)
	svc := service.NewService(ctx, config.Cfg, pub)

	// Start background workers
	go svc.Watcher().Start(ctx)
	go svc.Indexer().Start(ctx)
	go svc.Faces().StartScheduler(ctx)
	go svc.Embedder().Run(ctx)
	go svc.Rebuilder().MaybeAutoRebuild(svc.Indexer().MLReady)
	go svc.MountGuard().Run(ctx)

	// Prune orphaned TUS staging files at startup (one-shot) and then daily.
	go func() {
		if n, err := service.PruneStaging(common.StagingDir, time.Duration(common.StagingMaxAge)*time.Hour); err != nil {
			zap.L().Warn("PruneStaging failed", zap.Error(err))
		} else if n > 0 {
			zap.L().Info("PruneStaging removed orphans", zap.Int("count", n))
		}
	}()
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			service.PruneStaging(common.StagingDir, time.Duration(common.StagingMaxAge)*time.Hour) //nolint:errcheck
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
