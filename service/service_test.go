package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// TestDBSupportsPragmaOptimize exercises the exact statement main.go's daily
// maintenance ticker now runs alongside Storage().Prune (see the B5 debt-sweep
// item): Services.DB() already exposes the shared *sql.DB handle to main.go,
// so no new pass-through method was needed — this pins that running PRAGMA
// optimize against a normal migrated, populated DB succeeds, which is what
// the ticker callback relies on. The ticker's own goroutine/24h-interval
// plumbing has no existing test precedent in this codebase (main.go is
// package main and untested) and isn't exercised here.
func TestDBSupportsPragmaOptimize(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a.jpg','indexed')`)
	require.NoError(t, err)

	_, err = db.Exec(`PRAGMA optimize;`)
	require.NoError(t, err)
}

// TestServicesExposesGeo asserts that Services built by NewService can get a non-nil GeoService via Geo().
func TestServicesExposesGeo(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0",
		Workers:    1,
		WatchDirs:  nil,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(ctx, cfg, func(Task) {})

	require.NotNil(t, svc.Geo())
}

// TestNewService_TaskPublisherWired asserts NewService wires the publisher
// into TaskRegistry, so the callback fires once registry.Upsert goes through.
func TestNewService_TaskPublisherWired(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0", // won't actually connect
		Workers:    1,
		WatchDirs:  nil,
	}

	var mu sync.Mutex
	var got []Task
	pub := func(t Task) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, t)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(ctx, cfg, pub)

	svc.Tasks().Upsert(Task{
		ID: "t1", Type: "index", Label: "Indexing photos",
		Status: "running", Progress: 0.1, StartedAt: time.Now(),
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	}, time.Second, 10*time.Millisecond)
}

// TestNewService_BatchDoneTriggersFacePipeline asserts that the batch-done
// hook (SetOnBatchDone) triggers FaceService.RunPipeline rather than the old
// RunClustering: runs a real single-file batch; once the asset lands,
// face_scanned=0 (face detection has been moved out of the indexing
// pipeline), so RunClustering, facing 0 face_detections rows, won't emit any
// task at all; only RunPipeline emits a "face" task because a
// face_scanned=0 pending-detection asset exists (even if the ML endpoint is
// unavailable and per-image detection fails, the task is still created/completed as normal — this lets us tell the two apart).
func TestNewService_BatchDoneTriggersFacePipeline(t *testing.T) {
	tmp := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEGNamed(t, imgDir, "batch1.jpg")

	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0", // won't actually connect; per-image detection fails but doesn't block task creation
		Workers:    1,
		WatchDirs:  nil,
	}

	var mu sync.Mutex
	var got []Task
	pub := func(t Task) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, t)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(ctx, cfg, pub)

	svc.Indexer().SetIngestIdleTimeout(200 * time.Millisecond)
	go svc.Indexer().Start(ctx)
	svc.Indexer().EnqueueWithBatch(imgPath, "b1", 1)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, tk := range got {
			if tk.Type == "face" {
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "after batch completion, RunPipeline should emit a face task (RunClustering silently emits nothing when facing 0 face_detections)")
}

// TestNewService_BatchDoneTriggersEmbedBackfill asserts the batch-done hook
// also triggers the Embedder.Backfill backstop: during indexing, ML
// cold-loading/worker reclamation can cause embedClip to occasionally fail
// and get swallowed, and the recovery chain only triggers on an ML
// offline→recovered transition — if ML stays online the whole time, nobody
// catches up, leaving assets permanently missing vectors and unsearchable by
// semantic search (real incident: two fish photos). In this test case the ML
// endpoint is unreachable, so embedClip is bound to fail, and an "embedding" catch-up task must appear after the batch completes.
func TestNewService_BatchDoneTriggersEmbedBackfill(t *testing.T) {
	tmp := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEGNamed(t, imgDir, "batch-embed.jpg")

	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0",
		Workers:    1,
		WatchDirs:  nil,
	}

	var mu sync.Mutex
	var got []Task
	pub := func(t Task) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, t)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(ctx, cfg, pub)

	svc.Indexer().SetIngestIdleTimeout(200 * time.Millisecond)
	go svc.Indexer().Start(ctx)
	svc.Indexer().EnqueueWithBatch(imgPath, "b-embed", 1)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, tk := range got {
			if tk.Type == "embedding" {
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "batch completion should trigger a CLIP catch-up (embedding task), covering embedClip failures swallowed during indexing")
}
