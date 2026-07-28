// 共用选优测试:覆盖简报 Step 1 清单——内存库造 aesthetic_score + 注入可控
// 向量读取,断言连拍(60s 窗内向量余弦 > 0.95)只留最高分、featured 数量按
// maxFeatured 截断、cover 为分数最高者、向量缺失的资产跳过去重直接入池。
package service

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// noCoverLoader 模拟"读不到缩略图"(照 moments_test.go 的 noVecLoader 同款
// 命名范式),用于不关心亮度闸、只想验证 featured/排序逻辑的用例——loader
// 返回 false 时 pickCover 直接采用该候选,等价于跳过亮度闸。
func noCoverLoader(_ string) (image.Image, bool) { return nil, false }

// fakeCoverLoader 从预设 map 里查图像;不在 map 里的资产视为"读不到"。
func fakeCoverLoader(imgs map[string]image.Image) coverImageLoader {
	return func(assetID string) (image.Image, bool) {
		img, ok := imgs[assetID]
		return img, ok
	}
}

// solidGrayImage 造一张 w x h、灰度值全为 gray(0-255)的图片,用于亮度闸测试
// 里的"过暗/过曝/低对比灰雾"三种候选。
func solidGrayImage(w, h int, gray uint8) image.Image {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: gray})
		}
	}
	return img
}

// checkerImage 造一张黑白棋盘格图片(均值 ~0.5、标准差高),代表亮度/对比都
// 正常、能通过闸的"正常照片"。
func checkerImage(w, h int) image.Image {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				img.SetGray(x, y, color.Gray{Y: 0})
			} else {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	return img
}

// insertPickAsset 插入一条带 taken_at + aesthetic_score 的资产,供
// PickFeaturedAndCover 测试使用。
func insertPickAsset(t *testing.T, db *sql.DB, id string, takenAt time.Time, aesthetic float64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at, aesthetic_score) VALUES(?,?,'indexed',?,?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"), aesthetic)
	require.NoError(t, err)
}

// fakeVecLoader 从预设 map 里查向量;不在 map 里的资产视为"无向量"。
func fakeVecLoader(vecs map[string][]float32) clipVecLoader {
	return func(assetID string) ([]float32, bool) {
		v, ok := vecs[assetID]
		return v, ok
	}
}

func TestPickFeaturedAndCover_BurstDedupAndVectorlessSkip(t *testing.T) {
	db := makeTestDB(t)

	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)

	// 簇 A(60s 窗内的一次连拍):b1/b2 向量高度相似(>0.95)判连拍,只留美学分
	// 更高的 b2;b3 向量正交,不与 b1/b2 同组,独立入池。
	insertPickAsset(t, db, "b1", t0, 0.9)
	insertPickAsset(t, db, "b2", t0.Add(10*time.Second), 0.95)
	insertPickAsset(t, db, "b3", t0.Add(20*time.Second), 0.5)

	// 簇 B(与簇 A 相隔远超 60s,自成一簇):d1 无向量、e1 有向量,即便二者彼此
	// 相邻(10s 内),d1 因为没有向量必须跳过去重步骤直接入池,不与 e1 合并。
	t1 := t0.Add(2000 * time.Second)
	insertPickAsset(t, db, "d1", t1, 0.4)
	insertPickAsset(t, db, "e1", t1.Add(10*time.Second), 0.6)

	// 簇 C:孤立的单张,美学分全场最高,应作为 cover。
	t2 := t1.Add(2000 * time.Second)
	insertPickAsset(t, db, "c1", t2, 0.99)

	vecs := map[string][]float32{
		"b1": {1, 0},
		"b2": {1, 0.1}, // 与 b1 余弦相似度约 0.995 > 0.95,判连拍
		"b3": {0, 1},   // 与 b1/b2 都正交,不判连拍
		"e1": {0.5, 0.5},
		// d1、c1 故意不放入向量表:d1 测试"无向量跳过去重直接入池";
		// c1 本就是簇内唯一成员,无论是否有向量都会单独入池,顺带验证
		// "缺向量"不影响其入选。
	}

	assets := []MomentAsset{
		{AssetID: "b1"}, {AssetID: "b2"}, {AssetID: "b3"},
		{AssetID: "d1"}, {AssetID: "e1"}, {AssetID: "c1"},
	}

	loadVec := fakeVecLoader(vecs)

	// maxFeatured=3:候选池(去重后)按 aesthetic_score 降序为
	// c1(0.99) > b2(0.95) > e1(0.6) > b3(0.5) > d1(0.4),取前 3。
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 3, loadVec, noCoverLoader)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1"}, featured)
	require.Equal(t, "c1", cover)

	// 不截断(maxFeatured 足够大):完整候选池应为 5 个,b1 因连拍去重被剔除。
	full, cover2, err := PickFeaturedAndCover(context.Background(), db, assets, 10, loadVec, noCoverLoader)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1", "b3", "d1"}, full)
	require.Equal(t, "c1", cover2)
	require.NotContains(t, full, "b1", "连拍组内非最高分应被剔除出精选候选池")
}

func TestPickFeaturedAndCover_EmptyAssets(t *testing.T) {
	db := makeTestDB(t)
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, nil, 12, fakeVecLoader(nil), noCoverLoader)
	require.NoError(t, err)
	require.Empty(t, featured)
	require.Empty(t, cover)
}

// TestPickFeaturedAndCover_CoverPrefersScore 覆盖 theme/pet 场景:featured 候选
// 里美学分最高的不是 MomentAsset.Score 最高的,cover 应该跟着 score 走(CLIP
// 主题相似分),而不是 aesthetic_score——这正是"Your Beagle 封面挑最像狗的
// 那张"的诉求。全程用 noCoverLoader 跳过亮度闸,只验证排序。
func TestPickFeaturedAndCover_CoverPrefersScore(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	// 三张互不相邻(间隔远超 60s 连拍窗),各自独立入池。
	insertPickAsset(t, db, "hi-aesthetic", t0, 0.99)
	insertPickAsset(t, db, "hi-score", t0.Add(2000*time.Second), 0.5)
	insertPickAsset(t, db, "low-both", t0.Add(4000*time.Second), 0.1)

	assets := []MomentAsset{
		{AssetID: "hi-aesthetic", Score: 0.2},
		{AssetID: "hi-score", Score: 0.9}, // 主题相似分全场最高
		{AssetID: "low-both", Score: 0.1},
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, noCoverLoader)
	require.NoError(t, err)
	// featured 逻辑不变:仍按 aesthetic_score 降序。
	require.Equal(t, []string{"hi-aesthetic", "hi-score", "low-both"}, featured)
	// 但 cover 跟着 score 走,不是 featured[0]。
	require.Equal(t, "hi-score", cover)
}

// TestPickFeaturedAndCover_CoverSkipsDarkCandidate 覆盖亮度闸的"过暗"分支:
// score 最高的候选缩略图过暗,应被跳过,cover 落到次高分且通过闸的候选。
func TestPickFeaturedAndCover_CoverSkipsDarkCandidate(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "dark-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "dark-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	imgs := map[string]image.Image{
		"dark-top":      solidGrayImage(4, 4, 5), // 5/255 ≈ 0.02,过暗
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"dark-top", "normal-second"}, featured, "featured 排序不受亮度闸影响")
	require.Equal(t, "normal-second", cover, "过暗候选应被亮度闸跳过")
}

// TestPickFeaturedAndCover_CoverSkipsLowContrastCandidate 覆盖"灰雾/低对比"
// 分支:候选缩略图亮度均值正常但标准差过低(近乎纯色平面),应被跳过。
func TestPickFeaturedAndCover_CoverSkipsLowContrastCandidate(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "foggy-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "foggy-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	imgs := map[string]image.Image{
		"foggy-top":     solidGrayImage(4, 4, 128), // 均值 0.5 正常,但纯色标准差=0
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"foggy-top", "normal-second"}, featured)
	require.Equal(t, "normal-second", cover, "低对比灰雾候选应被亮度闸跳过")
}

// TestPickFeaturedAndCover_CoverAllRejectedFallsBackToFeaturedFirst 覆盖全拒
// 回退:所有候选都过不了亮度闸时,cover 应回退到原 featured[0]
// (aesthetic_score 序下的最高分),而不是"没有封面"。
func TestPickFeaturedAndCover_CoverAllRejectedFallsBackToFeaturedFirst(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "a", t0, 0.9)
	insertPickAsset(t, db, "b", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "a", Score: 0.5},
		{AssetID: "b", Score: 0.9},
	}
	imgs := map[string]image.Image{
		"a": solidGrayImage(4, 4, 5),   // 过暗
		"b": solidGrayImage(4, 4, 250), // 过曝
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, featured, "featured[0] 应为 aesthetic 序最高的 a")
	require.Equal(t, "a", cover, "全部候选被拒时应回退 featured[0]")
}

// TestPickFeaturedAndCover_CoverLoaderMissingParticipates 覆盖 loader 读不到
// 缩略图的分支:该候选应跳过亮度闸直接参选(而不是被剔除),分数最高、又读
// 不到图的候选依然当选 cover。
func TestPickFeaturedAndCover_CoverLoaderMissingParticipates(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "no-thumb-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "no-thumb-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	// imgs 里故意不放 no-thumb-top:模拟缩略图缺失/解码失败。
	imgs := map[string]image.Image{
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"no-thumb-top", "normal-second"}, featured)
	require.Equal(t, "no-thumb-top", cover, "loader 读不到应跳闸直接采用该候选")
}
