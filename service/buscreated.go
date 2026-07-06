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
		// 异步处理:大目录 walkSupported 可能跑数十秒,同步执行会停读本连接,
		// 而 MessageBus 服务端对读慢的订阅者是非阻塞发送、直接丢事件。
		// Enqueue 幂等(seen 去重)、队列有界,并发多个 walk 无害。
		// deleted 订阅保持同步(其处理是快速 DB 清理,不受此影响)。
		go handleCreatedPaths(ctx, ix, paths)
	})
}

// handleCreatedPaths enqueues landed files for indexing. Directories are
// expanded recursively (整目录复制/移动时发布方只发目的地根路径)。
// Enqueue 自带 seen 去重,与 fsnotify 重复触发无副作用。
func handleCreatedPaths(ctx context.Context, ix *Indexer, paths []string) {
	for _, p := range paths {
		select {
		case <-ctx.Done():
			return
		default:
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			// 已知限制:p 本身是「指向目录的符号链接」时 WalkDir(Lstat 语义)不下钻,
			// 交给周期全盘扫描兜底;这里不解引用,避免 symlink 环。
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
