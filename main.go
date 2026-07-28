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

	// 暂存跟随 DataPath(见 common.StagingDir 注释);必须在任何使用方(路由
	// 初始化、PruneStaging)之前完成重设。
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

	// 暂存目录含未完成上传的原始文件,权限收紧为 0700(与 route/v1/tus.go 的
	// 防御性建目录一致;MkdirAll 不会修改已存在目录的权限,故必须在这里就以
	// 0700 创建,不能进上面的 0755 共享列表)。
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

	// 存量视频悬浮预览雪碧图补跑(启动一次,CAS 防重入):覆盖升级前索引、
	// 未走新版内联预生成的历史视频。
	go svc.Indexer().BackfillSprites(ctx)

	// 启动时一次性清理孤儿 TUS 暂存文件(新旧两个目录都扫,旧目录只在启动时
	// 兜底扫一轮);此后每日清理由下方 ticker 负责(仅扫新目录)。
	go func() {
		for _, dir := range []string{common.StagingDir, common.LegacyStagingDir} {
			if n, err := service.PruneStaging(dir, time.Duration(common.StagingMaxAge)*time.Hour); err != nil {
				zap.L().Warn("PruneStaging failed", zap.String("dir", dir), zap.Error(err))
			} else if n > 0 {
				zap.L().Info("PruneStaging removed orphans", zap.String("dir", dir), zap.Int("count", n))
			}
		}
	}()

	// 每日全量缓存清理:孤儿缩略图目录 + face-thumbs 孤儿 + 过期暂存,
	// 与设置页手动按钮(POST /cache/prune)同一实现。
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if res, err := svc.Storage().Prune(common.StagingDir, time.Duration(common.StagingMaxAge)*time.Hour); err != nil {
				zap.L().Warn("daily cache prune failed", zap.Error(err))
			} else if res.RemovedCount > 0 {
				zap.L().Info("daily cache prune", zap.Int("removed", res.RemovedCount), zap.Int64("freed_bytes", res.FreedBytes))
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
