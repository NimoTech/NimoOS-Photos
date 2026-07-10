package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessFileInternal_InlineSpritePregen 通过真实索引管线（Enqueue→Start
// 的完整异步流程）验证 processFileInternal 第 8 步后触发的雪碧图预生成
// goroutine：视频入库后应异步落地 <thumbDir>/<assetID>/sprite.jpg，不必等到
// /sprite 路由的首次 hover 现场生成，也不依赖 BackfillSprites 补跑。
//
// 这条路径此前完全没有测试覆盖——sprite_backfill_test.go 只测 BackfillSprites
// 补跑逻辑，sprite_test.go 只测生成器本身；indexer.go 里 fire-and-forget 的
// 内联 goroutine 从未被真实索引管线（Enqueue/Start）触发过。
//
// 用 require.Eventually 轮询落地断言，确保测试在 goroutine 真正完成（或
// deadline 超时失败）之前不会返回——这样规避了 fire-and-forget 设计在 TempDir
// 测试环境下的已知竞态：若不等待就返回，t.TempDir() 清理可能先于 goroutine
// 写文件发生。
func TestProcessFileInternal_InlineSpritePregen(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	ix := NewIndexer(db, &mockML{}, thumbDir, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ix.Start(ctx)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25", "-y", src).Run())

	ix.Enqueue(src)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, src).Scan(&assetID) == nil
	}, 10*time.Second, 100*time.Millisecond, "视频应被索引管线处理完成")
	require.NotEmpty(t, assetID)

	spritePath := filepath.Join(thumbDir, assetID, "sprite.jpg")
	require.Eventually(t, func() bool {
		fi, err := os.Stat(spritePath)
		return err == nil && fi.Size() > 0
	}, 10*time.Second, 100*time.Millisecond, "视频入库应异步预生成悬浮雪碧图 sprite.jpg")
}
