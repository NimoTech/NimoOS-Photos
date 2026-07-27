// 共用选优测试:覆盖简报 Step 1 清单——内存库造 aesthetic_score + 注入可控
// 向量读取,断言连拍(60s 窗内向量余弦 > 0.95)只留最高分、featured 数量按
// maxFeatured 截断、cover 为分数最高者、向量缺失的资产跳过去重直接入池。
package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 3, loadVec)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1"}, featured)
	require.Equal(t, "c1", cover)

	// 不截断(maxFeatured 足够大):完整候选池应为 5 个,b1 因连拍去重被剔除。
	full, cover2, err := PickFeaturedAndCover(context.Background(), db, assets, 10, loadVec)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1", "b3", "d1"}, full)
	require.Equal(t, "c1", cover2)
	require.NotContains(t, full, "b1", "连拍组内非最高分应被剔除出精选候选池")
}

func TestPickFeaturedAndCover_EmptyAssets(t *testing.T) {
	db := makeTestDB(t)
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, nil, 12, fakeVecLoader(nil))
	require.NoError(t, err)
	require.Empty(t, featured)
	require.Empty(t, cover)
}
