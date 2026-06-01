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
