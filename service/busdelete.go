package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Common/external"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// busEvent matches the JSON envelope pushed by NimoOS-MessageBus.
type busEvent struct {
	SourceID   string            `json:"sourceID"`
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
	Timestamp  interface{}       `json:"timestamp"`
	UUID       string            `json:"uuid"`
}

// extractEventPaths parses a MessageBus event envelope and returns the list
// of absolute file/directory paths encoded in properties["paths"].
// Only messages whose Name matches eventName are handled; any other
// message (heartbeat, different event) returns nil. Parse errors also return nil.
func extractEventPaths(message []byte, eventName string) []string {
	var ev busEvent
	if err := json.Unmarshal(message, &ev); err != nil {
		return nil
	}
	if ev.Name != eventName {
		return nil
	}
	rawPaths, ok := ev.Properties["paths"]
	if !ok || rawPaths == "" {
		return nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(rawPaths), &paths); err != nil {
		return nil
	}
	return paths
}

// shouldHandleDeletedPath reports whether a deleted-path notification should
// trigger a DB lookup. The filter is conservative: we only skip paths that are
// clearly non-media files (have an extension that is not in supportedExts).
// Paths without an extension are treated as potential directories and passed
// through, as are all recognised media extensions (case-insensitive).
func shouldHandleDeletedPath(p string) bool {
	base := filepath.Base(p)
	ext := strings.ToLower(filepath.Ext(base))
	if ext == "" {
		// No extension — likely a directory; must handle.
		return true
	}
	return supportedExts[ext]
}

// handleDeletedPaths cleans up indexed assets and their CLIP vectors for a
// slice of deleted paths. For each path that passes shouldHandleDeletedPath:
//   - RemoveByPath removes an exact-match asset row (file delete case).
//   - pruneMissingUnder removes any remaining assets under the path whose
//     files no longer exist on disk (directory delete / partial removal case).
//
// Both helpers carry their own vector + thumbnail cleanup, so no extra steps
// are needed here.
func handleDeletedPaths(ix *Indexer, paths []string) {
	for _, p := range paths {
		if !shouldHandleDeletedPath(p) {
			continue
		}
		ix.RemoveByPath(p)
		_ = ix.pruneMissingUnder(p)
	}
}

// busWsURL converts the HTTP address returned by external.GetMessageBusAddress
// (e.g. "http://127.0.0.1:8090/v2/message_bus" or "127.0.0.1:8090/v2/message_bus")
// into the WebSocket subscription URL for the given event name.
func busWsURL(busAddr, eventName string) string {
	// Strip trailing slash for consistency.
	busAddr = strings.TrimRight(busAddr, "/")

	// Strip the /v2/message_bus suffix so we can re-build the WS path.
	// GetMessageBusAddress always appends APIMessageBus ("/v2/message_bus").
	busAddr = strings.TrimSuffix(busAddr, external.APIMessageBus)

	// Convert scheme: http → ws, https → wss; bare host:port gets ws://.
	var wsBase string
	switch {
	case strings.HasPrefix(busAddr, "https://"):
		wsBase = "wss://" + busAddr[len("https://"):]
	case strings.HasPrefix(busAddr, "http://"):
		wsBase = "ws://" + busAddr[len("http://"):]
	default:
		wsBase = "ws://" + busAddr
	}

	// Subscription route (verified against the bus router and a live 101
	// handshake): GET /v2/message_bus/event/{source_id}?names=... upgrades to
	// WebSocket. Note: it is "event" (singular), with no "/ws" suffix.
	return wsBase + "/v2/message_bus/event/nimoos?names=" + eventName
}

// runBusPathsSubscriber connects to the MessageBus via WebSocket and listens
// for events named eventName, invoking handle with the list of paths reported
// in each event's properties["paths"].
//
// This is the real-time layer. Periodic full-disk scans (ScanAllRoots /
// pruneMissingUnder) remain the durable safety net — this subscriber
// complements them by reacting within milliseconds so that changes do not
// linger unreflected until the next scheduled scan.
//
// The function runs inside a goroutine (the caller is responsible for the go
// statement). On connection failure or disconnection it backs off with
// exponential delay (5 s → 10 s → … → 60 s max) and retries automatically.
// It exits cleanly when ctx is cancelled. All errors are logged at Warn level;
// the function never panics.
func runBusPathsSubscriber(ctx context.Context, runtimePath, eventName string, handle func(paths []string)) {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff

	for {
		// Respect context cancellation before each attempt.
		select {
		case <-ctx.Done():
			return
		default:
		}

		busAddr, err := external.GetMessageBusAddress(runtimePath)
		if err != nil {
			zap.L().Warn("bus subscriber: cannot resolve MessageBus address",
				zap.String("runtimePath", runtimePath),
				zap.Error(err))
			if !sleepOrCancel(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		wsURL := busWsURL(busAddr, eventName)
		zap.L().Info("bus subscriber: connecting to MessageBus",
			zap.String("url", wsURL),
			zap.String("event", eventName))

		conn, _, dialErr := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if dialErr != nil {
			zap.L().Warn("bus subscriber: WebSocket dial failed",
				zap.String("url", wsURL),
				zap.Error(dialErr))
			if !sleepOrCancel(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		// Reset backoff on successful connection.
		backoff = initialBackoff
		zap.L().Info("bus subscriber: connected to MessageBus", zap.String("event", eventName))

		// Start a goroutine that closes the connection when ctx is cancelled so
		// that conn.ReadMessage() unblocks and the read loop exits promptly.
		stopRead := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-stopRead:
			}
		}()

		// Read loop: process messages until the connection drops or ctx is done.
		readErr := func() error {
			for {
				msgType, data, err := conn.ReadMessage()
				if err != nil {
					return err
				}
				if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
					// Ping/Pong/Close frames — gorilla handles Ping automatically.
					continue
				}
				paths := extractEventPaths(data, eventName)
				if len(paths) > 0 {
					handle(paths)
				}
			}
		}()

		close(stopRead)
		conn.Close()

		if ctx.Err() != nil {
			// Context cancelled — clean exit.
			return
		}

		zap.L().Warn("bus subscriber: WebSocket connection lost; will retry",
			zap.String("event", eventName),
			zap.Error(readErr),
			zap.Duration("backoff", backoff))
		if !sleepOrCancel(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// StartMediaDeletedSubscriber connects to the MessageBus via WebSocket and
// listens for "nimoos:media:deleted" events, immediately cleaning up any indexed
// assets and CLIP vectors for the reported paths.
func StartMediaDeletedSubscriber(ctx context.Context, runtimePath string, ix *Indexer) {
	runBusPathsSubscriber(ctx, runtimePath, "nimoos:media:deleted", func(paths []string) {
		handleDeletedPaths(ix, paths)
	})
}

// sleepOrCancel sleeps for d or returns false immediately when ctx is done.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// nextBackoff doubles the current backoff, capped at max.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		next = max
	}
	return next
}
