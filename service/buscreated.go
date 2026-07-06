package service

import (
	"context"
	"os"

	"go.uber.org/zap"
)

// StartMediaCreatedSubscriber subscribes to nimoos:media:created on its OWN
// WS connection. 独立连接是刻意的:主服务未升级(事件未注册)时本订阅会被
// MessageBus 以 400 拒绝并退避重试,但绝不能连累 deleted 订阅那条连接。
func StartMediaCreatedSubscriber(ctx context.Context, runtimePath string, ix *Indexer) {
	runBusPathsSubscriber(ctx, runtimePath, "nimoos:media:created", func(paths []string) {
		handleCreatedPaths(ix, paths)
	})
}

// handleCreatedPaths enqueues landed files for indexing. Directories are
// expanded recursively (整目录复制/移动时发布方只发目的地根路径)。
// Enqueue 自带 seen 去重,与 fsnotify 重复触发无副作用。
func handleCreatedPaths(ix *Indexer, paths []string) {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			if werr := walkSupported(p, func(f string) { ix.Enqueue(f) }); werr != nil {
				zap.L().Warn("media:created: walk failed", zap.String("dir", p), zap.Error(werr))
			}
			continue
		}
		if isSupportedMedia(p) {
			ix.Enqueue(p)
		}
	}
}
