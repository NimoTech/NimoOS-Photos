package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// TestServicesExposesGeo 断言 NewService 构造的 Services 能通过 Geo() 拿到非 nil 的 GeoService。
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

// TestNewService_TaskPublisherWired 断言 NewService 把 publisher 接到 TaskRegistry
// 上，registry.Upsert 走通后回调被触发。
func TestNewService_TaskPublisherWired(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0", // 不会真的连
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
		ID: "t1", Type: "index", Label: "索引照片",
		Status: "running", Progress: 0.1, StartedAt: time.Now(),
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	}, time.Second, 10*time.Millisecond)
}

// TestNewService_BatchDoneTriggersFacePipeline 断言批次完成钩子(SetOnBatchDone)
// 触发的是 FaceService.RunPipeline 而非旧的 RunClustering：跑一个真实的单文件
// 批次，asset 落地后 face_scanned=0（人脸检测已移出索引流水线），RunClustering
// 面对 0 条 face_detections 会完全不发任务；只有 RunPipeline 会因为存在
// face_scanned=0 的待检测资产而发出一个 "face" 任务（哪怕 ML 端点不可用，检测
// 逐张失败也不影响任务照常创建/完成——可判定区分二者）。
func TestNewService_BatchDoneTriggersFacePipeline(t *testing.T) {
	tmp := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEGNamed(t, imgDir, "batch1.jpg")

	cfg := &config.Config{
		DataPath:   tmp,
		MLEndpoint: "http://127.0.0.1:0", // 不会真的连上，检测阶段逐张失败但不影响任务创建
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
	}, 10*time.Second, 50*time.Millisecond, "批次完成后应由 RunPipeline 发出 face 任务(RunClustering 面对 0 条 face_detections 会静默不发任务)")
}

// TestNewService_BatchDoneTriggersEmbedBackfill 断言批次完成钩子同时触发
// Embedder.Backfill 兜底:索引期间 ML 冷加载/worker 回收会让 embedClip 偶发
// 失败且被吞,恢复链只在 ML 掉线→恢复跳变时触发——ML 全程在线就没人补,
// 资产无限期缺向量、语义搜索搜不到(真实故障:两张鱼图)。本用例里 ML 端点
// 不可达,embedClip 必然失败,批次完成后必须出现 "embedding" 补跑任务。
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
	}, 10*time.Second, 50*time.Millisecond, "批次完成后应触发 CLIP 补跑(embedding 任务),兜住索引期间被吞的 embedClip 失败")
}
