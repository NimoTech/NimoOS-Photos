package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsInSnapshotsDir 覆盖共享判定函数的核心契约:必须是路径的一个完整分段
// 才算命中,单纯子串命中(如 "my.snapshots.backup")绝不能误判。
func TestIsInSnapshotsDir(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/media/RAID_0/.snapshots/20260716T200714Z_auto-hourly/Image/a.jpg", true},
		{"/media/RAID_0/.snapshots", true},
		{"/media/RAID_0/.snapshots/", true},
		{"/.snapshots/x", true},
		// 嵌套多层也必须命中:根本身在 .snapshots 更深处。
		{"/media/RAID_0/.snapshots/ts/sub/deeper/x.jpg", true},
		// 仅部分/子串匹配,不是独立路径分段,绝不能误判。
		{"/media/RAID_0/my.snapshots.backup/a.jpg", false},
		{"/media/RAID_0/.snapshotsfoo/a.jpg", false},
		{"/media/RAID_0/foo.snapshots/a.jpg", false},
		{"/media/RAID_0/normal/a.jpg", false},
		{"/DATA/Gallery/photo.jpg", false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, isInSnapshotsDir(c.path), "isInSnapshotsDir(%q)", c.path)
	}
}

// TestWalkSupportedSkipsNestedSnapshotsDir:正常扫描一个卷根目录时,途中遇到
// 名为 .snapshots 的子目录(不论嵌套多深)必须整体 SkipDir,同级/上级的正常
// 文件不受影响。
func TestWalkSupportedSkipsNestedSnapshotsDir(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep.jpg")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))

	// 模拟 <卷挂载点>/.snapshots/<ts>/Image/xxx.jpg 的真实快照目录形态,嵌套
	// 两层以证明不是只挡了 .snapshots 自身这一层。
	snapDeep := filepath.Join(root, ".snapshots", "20260716T200714Z_auto-hourly", "Image")
	require.NoError(t, os.MkdirAll(snapDeep, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapDeep, "a.jpg"), []byte("y"), 0o644))

	var collected []string
	require.NoError(t, walkSupported(context.Background(), root, func(p string) {
		collected = append(collected, p)
	}))

	require.Equal(t, []string{keep}, collected,
		".snapshots 目录树下的文件必须被整体跳过,只有正常文件被收集")
}

// TestWalkSupportedSkipsWhenRootItselfIsInsideSnapshots 覆盖真实事故的根因:
// btrbk/snapper 把每个快照子卷单独挂载,walk 的根本身(而不是遍历途中遇到的
// 子目录)就已经在 .snapshots 里面——这种情况下"遍历途中跳过"永远不会触发
// (根节点从不会被当成自己的子目录来判断),必须在入口处单独拦截。
func TestWalkSupportedSkipsWhenRootItselfIsInsideSnapshots(t *testing.T) {
	root := t.TempDir()
	snapRoot := filepath.Join(root, ".snapshots", "20260716T200714Z_auto-hourly")
	require.NoError(t, os.MkdirAll(filepath.Join(snapRoot, "Image"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapRoot, "Image", "a.jpg"), []byte("y"), 0o644))

	var collected []string
	require.NoError(t, walkSupported(context.Background(), snapRoot, func(p string) {
		collected = append(collected, p)
	}))

	require.Empty(t, collected, "walk 的根本身位于 .snapshots 之下时必须整体跳过,不收集任何文件")
}

// TestPruneSnapshotAssets 覆盖启动清理:既有污染行(路径含 /.snapshots/)必须
// 走完整的资产删除流程被清干净(含 CLIP 向量、人脸行),正常路径与"仅部分
// 匹配"的相似路径(my.snapshots.backup)必须原封不动地保留。
func TestPruneSnapshotAssets(t *testing.T) {
	db := makeTestDB(t)

	contaminated := insertAsset(t, db,
		"/media/RAID_0/.snapshots/20260716T200714Z_auto-hourly/Image/a.jpg", "indexed")
	seedFaceAndClip(t, db, contaminated)

	keep := insertAsset(t, db, "/media/RAID_0/Image/a.jpg", "indexed")
	seedFaceAndClip(t, db, keep)

	// 仅路径子串"看起来像"快照目录,实际是一个普通用户目录,绝不能被误删。
	lookalike := insertAsset(t, db, "/media/RAID_0/my.snapshots.backup/a.jpg", "indexed")
	seedFaceAndClip(t, db, lookalike)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSnapshotAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "快照目录下的污染资产必须被硬删")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "污染资产的 CLIP 映射必须被清")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "污染资产的人脸行必须被清")

	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n, "正常资产不得被波及")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, lookalike).Scan(&n))
	require.Equal(t, 1, n, "仅路径子串相似(my.snapshots.backup)的资产不得被误删")
}

// TestPruneSnapshotAssetsNoContamination 覆盖零命中场景:不能因为没有污染行
// 就出错或误删任何东西。
func TestPruneSnapshotAssetsNoContamination(t *testing.T) {
	db := makeTestDB(t)
	keep := insertAsset(t, db, "/DATA/Gallery/a.jpg", "indexed")

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSnapshotAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}
