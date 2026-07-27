// trip 引擎测试:覆盖简报 Step 1 清单——两段相隔 20 天的 GPS 簇产出 2 个
// draft、5 张小簇被 min_assets 过滤、双城命名、subtitle 格式(含跨月)、
// 同数据重算 id 稳定。
package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// insertTripAsset 插入一条满足候选池条件的资产(status='indexed'、非回收站、
// 无 OCR 即非文档)+ 对应 asset_geo 行,供 trip 引擎测试使用。
func insertTripAsset(t *testing.T, db *sql.DB, id string, takenAt time.Time, city, country string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, city, country)
	require.NoError(t, err)
}

// day 是构造测试时间戳的简写:2011 年 month 月 d 日 12:00 UTC。
func day(month time.Month, d int) time.Time {
	return time.Date(2011, month, d, 12, 0, 0, 0, time.UTC)
}

func TestBuildTripMoments_SplitFilterAndNaming(t *testing.T) {
	db := makeTestDB(t)

	// 段 A:东京 6 张,4 月 1 日~4 月 6 日(单城,同月)。
	segA := []time.Time{day(4, 1), day(4, 2), day(4, 3), day(4, 4), day(4, 5), day(4, 6)}
	for i, ts := range segA {
		insertTripAsset(t, db, "a"+string(rune('0'+i)), ts, "Tokyo", "Japan")
	}

	// 小簇:京都 5 张,与段 A 相隔 20 天(4/6 → 4/26),数量不足 min_assets=6,
	// 应被过滤。
	small := []time.Time{day(4, 26), day(4, 27), day(4, 28), day(4, 29), day(4, 30)}
	for i, ts := range small {
		insertTripAsset(t, db, "s"+string(rune('0'+i)), ts, "Kyoto", "Japan")
	}

	// 段 C:6 张,与小簇相隔 20 天(4/30 → 5/20),跨 5 月/6 月,双城
	// (Paris 4 张 + Lyon 2 张,Lyon 占比 2/6=33.3% > 30%)。
	segC := []struct {
		ts   time.Time
		city string
	}{
		{day(5, 20), "Paris"},
		{day(5, 22), "Paris"},
		{day(5, 24), "Paris"},
		{day(5, 26), "Paris"},
		{day(5, 30), "Lyon"},
		{time.Date(2011, 6, 4, 12, 0, 0, 0, time.UTC), "Lyon"},
	}
	for i, c := range segC {
		insertTripAsset(t, db, "c"+string(rune('0'+i)), c.ts, c.city, "France")
	}

	recipe := MomentRecipe{Key: "trip", Kind: "trip", ParamsJSON: `{"min_assets":6}`}

	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 2, "小簇应被 min_assets 过滤,只剩段 A + 段 C")

	dA, dC := drafts[0], drafts[1]

	// 段 A:单城、同月 subtitle、6 张成员。
	require.Equal(t, "Tokyo", dA.Place)
	require.Equal(t, "Tokyo Trip", dA.Title)
	require.Equal(t, "Apr 2011", dA.Subtitle)
	require.Len(t, dA.Assets, 6)
	require.True(t, dA.TimeFrom.Equal(segA[0]))
	require.True(t, dA.TimeTo.Equal(segA[len(segA)-1]))
	for _, a := range dA.Assets {
		require.False(t, a.Featured)
		require.Zero(t, a.Score)
	}

	// 段 C:双城命名 + 跨月 subtitle(en dash 两侧各一空格)。
	require.Equal(t, "Paris & Lyon", dC.Place)
	require.Equal(t, "Paris & Lyon Trip", dC.Title)
	require.Equal(t, "May – Jun 2011", dC.Subtitle)
	require.Len(t, dC.Assets, 6)
	require.True(t, dC.TimeFrom.Equal(segC[0].ts))
	require.True(t, dC.TimeTo.Equal(segC[len(segC)-1].ts))

	// id 稳定性:同一份数据重算应得到完全相同的 id 集合(不因重算顺序/走查
	// 而"改名换姓")。
	drafts2, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts2, 2)
	require.Equal(t, dA.ID, drafts2[0].ID)
	require.Equal(t, dC.ID, drafts2[1].ID)
	require.Equal(t, TripMomentID("trip", segA[0]), dA.ID)
	require.Equal(t, TripMomentID("trip", segC[0].ts), dC.ID)
}

func TestBuildTripMoments_EmptyPool(t *testing.T) {
	db := makeTestDB(t)
	recipe := MomentRecipe{Key: "trip", Kind: "trip"}
	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)
}

func TestBuildTripMoments_ExcludesTrashOfflineAndDocs(t *testing.T) {
	db := makeTestDB(t)

	// 6 张正常东京照片(达到默认 min_assets=10 以下的自定义阈值)。
	recipe := MomentRecipe{Key: "trip", Kind: "trip", ParamsJSON: `{"min_assets":6}`}
	base := []time.Time{day(7, 1), day(7, 2), day(7, 3), day(7, 4), day(7, 5), day(7, 6)}
	for i, ts := range base {
		insertTripAsset(t, db, "t"+string(rune('0'+i)), ts, "Tokyo", "Japan")
	}

	// 回收站资产(deleted_at 非空):不应计入候选池,也不应拖累 gap 判定。
	insertTripAsset(t, db, "trashed", day(7, 7), "Tokyo", "Japan")
	_, err := db.Exec(`UPDATE assets SET deleted_at=? WHERE id='trashed'`, "2011-07-08 00:00:00")
	require.NoError(t, err)

	// 离线资产(offline=1):同理排除。
	insertTripAsset(t, db, "offline1", day(7, 8), "Tokyo", "Japan")
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline1'`)
	require.NoError(t, err)

	// 文档类资产(hasOcrExpr 判据成立):密度闸达标(coverage/line_count)+
	// is_doc=1,排除。
	insertTripAsset(t, db, "doc1", day(7, 9), "Tokyo", "Japan")
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, is_doc)
		VALUES('doc1','some long ocr text with many lines',0.9,20,1)`)
	require.NoError(t, err)

	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)
	require.Len(t, drafts[0].Assets, 6, "回收站/离线/文档资产均不应计入候选池")
}
