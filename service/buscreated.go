package service

import (
	"context"
	"os"

	"go.uber.org/zap"
)

// StartMediaCreatedSubscriber subscribes to nimoos:media:created on its OWN
// WS connection. A separate connection is deliberate: if the main service
// hasn't been upgraded yet (event not registered), this subscription gets
// rejected by MessageBus with 400 and backs off/retries, but that must never
// take down the deleted-subscriber connection.
func StartMediaCreatedSubscriber(ctx context.Context, runtimePath string, ix *Indexer) {
	runBusPathsSubscriber(ctx, runtimePath, "nimoos:media:created", func(paths []string) {
		// Processed asynchronously: walkSupported on a large directory can run
		// for tens of seconds, and synchronous execution would stall reads on
		// this connection — MessageBus sends to slow-reading subscribers
		// non-blockingly and just drops events. Enqueue is idempotent (seen
		// dedup) and the queue is bounded, so concurrent walks are harmless.
		// The deleted subscriber stays synchronous (its handling is a quick
		// DB cleanup, unaffected by this).
		go handleCreatedPaths(ctx, ix, paths)
	})
}

// handleCreatedPaths enqueues landed files for indexing. Directories are
// expanded recursively (when an entire directory is copied/moved, the
// publisher only sends the destination root path). Enqueue has built-in
// seen dedup, so duplicate fsnotify triggers are harmless.
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
			// Known limitation: when p itself is a "symlink pointing to a
			// directory", WalkDir (Lstat semantics) won't descend into it;
			// the periodic full scan covers it as a fallback. We don't
			// dereference here, to avoid symlink loops.
			if werr := walkSupported(ctx, p, func(f string) { ix.Enqueue(f) }); werr != nil {
				zap.L().Warn("media:created: walk failed", zap.String("dir", p), zap.Error(werr))
			}
			continue
		}
		if isSupportedMedia(p) {
			ix.Enqueue(p)
		}
	}
}
