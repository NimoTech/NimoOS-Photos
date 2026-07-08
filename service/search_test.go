package service_test

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

type mockTextML struct{}

func (m *mockTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

func openSearchDB(t *testing.T) *service.SearchService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return service.NewSearchService(db, &mockTextML{})
}

func TestSmartSearch(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/beach.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid)

	vec := make([]float32, common.CLIPDim)
	vec[0] = 1.0
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "a1", results[0].ID)
}

// TestSmartSearchIncludeOCR 验证 IncludeOCR 开启时 OCR 子串命中以 1.0 分置顶、
// 与 CLIP 结果按 ID 去重，且默认（关闭）行为不变。
func TestSmartSearchIncludeOCR(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ocr.db"))
	require.NoError(t, err)
	defer db.Close()

	// a1: 只有 CLIP 向量；a2: 只有 OCR 文本；a3: 两者都有（去重场景）
	for i, id := range []string{"a1", "a2", "a3"} {
		db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at)
			VALUES(?,?,'indexed',0,?)`, id, "/p/"+id+".jpg", fmt.Sprintf("2025-07-%02d 10:00:00", i+1))
	}
	for _, id := range []string{"a1", "a3"} {
		db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		var rowid int64
		db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid)
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	}
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a2','SUPERMART Receipt TOTAL $42.00')`)
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a3','Invoice receipt 2024')`)

	svc := service.NewSearchService(db, &mockTextML{})

	// 默认关闭：纯 CLIP，OCR-only 的 a2 不出现
	results, err := svc.SmartSearch("receipt", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	for _, a := range results {
		require.NotEqual(t, "a2", a.ID, "IncludeOCR=false 时不应返回 OCR-only 命中")
	}

	// 开启：a2、a3 以 1.0 分置顶（OCR 组内按拍摄时间倒序 → a3 在前），a1 仍在
	results, err = svc.SmartSearch("receipt", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), 3)
	require.Equal(t, "a3", results[0].ID)
	require.Equal(t, "a2", results[1].ID)
	require.NotNil(t, results[0].MatchScore)
	require.InDelta(t, 1.0, *results[0].MatchScore, 1e-9)
	// OCR 命中带 matchedBy="ocr" 标记（双路命中时保留 OCR 版本）；纯 CLIP 命中带 "semantic"
	require.Equal(t, "ocr", results[0].MatchedBy)
	require.Equal(t, "ocr", results[1].MatchedBy)
	for _, a := range results {
		if a.ID == "a1" {
			require.Equal(t, "semantic", a.MatchedBy)
		}
	}
	ids := map[string]int{}
	for _, a := range results {
		ids[a.ID]++
	}
	require.Equal(t, 1, ids["a3"], "双路命中必须去重")
	require.Equal(t, 1, ids["a1"])

	// 大小写不敏感
	results, err = svc.SmartSearch("RECEIPT", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Equal(t, "a3", results[0].ID)
}

// TestSmartSearchBelowCutTiering 验证 SmartSearch 集成场景下的自适应断层落点：
// OCR 命中恒最佳层（不参与断点计算），语义命中的教科书断崖尾部（同规格 §2 的
// "fish" 示例：4 条真命中 0.86~0.66，随后断崖到 0.13 的噪声）被正确置 BelowCut。
func TestSmartSearchBelowCutTiering(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "cut.db"))
	require.NoError(t, err)
	defer db.Close()

	// OCR 命中：精确文本子串命中，恒 1.0 分，恒最佳层。
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at)
		VALUES('ocr1','/p/ocr1.jpg','indexed',0,'2025-07-01 10:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('ocr1','fish menu special')`)
	require.NoError(t, err)

	// seedSemantic 反解 displayScore 的线性映射（floor=0.03/ceil=0.13 默认值），
	// 构造出恰好产生目标展示分 ds 的 CLIP 向量（查询向量固定为 e0=[1,0,...]）。
	seedSemantic := func(id string, ds float64) {
		x := 0.03 + ds*0.10
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seedSemantic("s1", 0.86)
	seedSemantic("s2", 0.80)
	seedSemantic("s3", 0.72)
	seedSemantic("s4", 0.66)
	seedSemantic("tail1", 0.13)
	seedSemantic("tail2", 0.13)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("fish", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Len(t, results, 7)

	belowCut := map[string]bool{}
	matchedBy := map[string]string{}
	for _, a := range results {
		belowCut[a.ID] = a.BelowCut
		matchedBy[a.ID] = a.MatchedBy
	}
	require.Equal(t, "ocr", matchedBy["ocr1"])
	require.False(t, belowCut["ocr1"], "OCR 命中恒最佳层，不参与断点计算")
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		require.Equal(t, "semantic", matchedBy[id])
		require.False(t, belowCut[id], id+" 应在最佳匹配层")
	}
	for _, id := range []string{"tail1", "tail2"} {
		require.Equal(t, "semantic", matchedBy[id])
		require.True(t, belowCut[id], id+" 应折入 more-results 折叠层")
	}
}

// TestSmartSearchNoBelowCutWhenFewResults 验证边界守卫：语义结果少于 3 条时不分层
// （全部落在最佳匹配层），即便分差很大。
func TestSmartSearchNoBelowCutWhenFewResults(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "few.db"))
	require.NoError(t, err)
	defer db.Close()

	seed := func(id string, x float64) {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seed("hi", 0.12)
	seed("lo", 0.031)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("fish", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, a := range results {
		require.False(t, a.BelowCut, a.ID+"：语义结果<3 条时不应分层")
	}
}

// seedRankedSemantic 依次插入 n 条语义资产 id0..id{n-1}，通过反解 displayScore 的
// 线性映射构造严格递减的分数，从而固定 KNN 排序（id0 分最高排最前）。
func seedRankedSemantic(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%d", i)
		ds := 0.95 - float64(i)*0.03 // 严格递减，彼此分差足够避免 cut/顺序歧义
		x := 0.03 + ds*0.10
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
}

// TestSmartSearchOffsetPaginationMatchesFullList 验证分页切片正确性：offset>0 页
// 的内容必须等于「一次性取全量列表」对应区段的资产（同一份 KNN 排序上切片）。
func TestSmartSearchOffsetPaginationMatchesFullList(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "page.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 12)

	svc := service.NewSearchService(db, &mockTextML{})
	full, err := svc.SmartSearch("fish", 12, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, full, 12)

	page, err := svc.SmartSearch("fish", 5, 5, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, page, 5)
	for i, a := range page {
		require.Equal(t, full[5+i].ID, a.ID, "第二页第 %d 条应等于全量列表第 %d 条", i, 5+i)
	}
}

// TestSmartSearchOffsetPageAllBelowCutNoOCR 验证 offset>0 时：不做 OCR 前置合并
// （即便 IncludeOCR=true，纯 OCR 命中也不会出现在深页），且全部结果 BelowCut=true。
func TestSmartSearchOffsetPageAllBelowCutNoOCR(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "page_ocr.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 8)
	// 一条纯 OCR 命中：文本命中但没有 CLIP 向量，首页会被前置合并进来。
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('ocr1','/p/ocr1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('ocr1','fish menu special')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, &mockTextML{})

	// 首页：OCR 命中应出现且恒最佳层。
	first, err := svc.SmartSearch("fish", 3, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	var sawOCR bool
	for _, a := range first {
		if a.MatchedBy == "ocr" {
			sawOCR = true
			require.False(t, a.BelowCut)
		}
	}
	require.True(t, sawOCR, "首页应包含 OCR 前置命中")

	// 深页：跳过 OCR 合并，不应再出现 ocr1；全部结果 BelowCut=true。
	deep, err := svc.SmartSearch("fish", 3, 3, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.NotEmpty(t, deep)
	for _, a := range deep {
		require.NotEqual(t, "ocr1", a.ID, "深页不应做 OCR 前置合并")
		require.Equal(t, "semantic", a.MatchedBy)
		require.True(t, a.BelowCut, a.ID+"：offset>0 的结果应全部置 belowCut=true")
	}
}

// TestSmartSearchOffsetBeyondLibraryReturnsActualCount 验证库不足时（offset+limit
// 超出库存量）返回实际数量而非报错或补齐空结果。
func TestSmartSearchOffsetBeyondLibraryReturnsActualCount(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "short.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 4)

	svc := service.NewSearchService(db, &mockTextML{})

	// offset 落在库存量内，但 offset+limit 超出：只应拿到剩余的那几条。
	partial, err := svc.SmartSearch("fish", 10, 3, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, partial, 1)
	require.Equal(t, "id3", partial[0].ID)

	// offset 本身就超出库存量：返回空切片，不报错。
	empty, err := svc.SmartSearch("fish", 10, 100, service.SearchFilters{})
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestSmartSearchNegativeOffsetClampsToZero 验证 service 层对负数 offset 的防御性
// 归零（路由层已经归零，这里确保 SmartSearch 本身也不会因负数 offset 产生异常行为，
// 例如切片越界或 k 值被算小）。
func TestSmartSearchNegativeOffsetClampsToZero(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "negoff.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 5)

	svc := service.NewSearchService(db, &mockTextML{})
	withNeg, err := svc.SmartSearch("fish", 5, -3, service.SearchFilters{})
	require.NoError(t, err)
	withZero, err := svc.SmartSearch("fish", 5, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, len(withZero), len(withNeg))
	for i := range withZero {
		require.Equal(t, withZero[i].ID, withNeg[i].ID)
	}
}

func TestTimeline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tl.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('v1','/p1.mov','indexed','2025-07-15 10:00:00',1)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, 2025, groups[0].Year)
	require.Equal(t, 7, groups[0].Month)
	require.Len(t, groups[0].Assets, 1, "live photo video must be hidden")
}

// TestTimelineEnrichesPlaceName verifies Timeline/ListAssets surface a city-level
// PlaceName ("City, Country") from asset_geo, so the client filters by city rather
// than falling back to a coordinate-derived country.
func TestTimelineEnrichesPlaceName(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "place.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a2','/p2.jpg','indexed','2025-07-15 11:00:00',0)`)
	// a1 is geocoded to Tokyo, Japan; a2 has no geo row → PlaceName stays empty.
	db.Exec(`INSERT INTO asset_geo(asset_id,city_id,city,country,region,lat,lon) VALUES('a1',1850147,'Tokyo','Japan','asia',35.68,139.69)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	got := map[string]string{}
	for _, g := range groups {
		for _, a := range g.Assets {
			got[a.ID] = a.PlaceName
		}
	}
	require.Equal(t, "Tokyo, Japan", got["a1"])
	require.Equal(t, "", got["a2"])

	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	for _, a := range assets {
		if a.ID == "a1" {
			require.Equal(t, "Tokyo, Japan", a.PlaceName)
		}
	}
}

// TestSmartSearchExcludesOffline 验证:资产所在可移动盘被拔出(offline=1)时,
// 即使有 CLIP 向量也不应出现在语义搜索结果里,如同暂时不存在。
func TestSmartSearchExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "so.db"))
	require.NoError(t, err)
	defer db.Close()

	seed := func(id, path string) {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES(?,?,'indexed',0)`, id, path)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seed("online", "/DATA/beach.jpg")
	seed("offline", "/media/X/beach2.jpg")
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, a := range results {
		ids[a.ID] = true
	}
	require.True(t, ids["online"], "在线资产应出现在结果中")
	require.False(t, ids["offline"], "offline 资产必须从语义搜索结果中隐藏")
}

// TestTimelineExcludesOffline 验证时间线视图隐藏 offline=1 的资产。
func TestTimelineExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tlo.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('online','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed','2025-07-15 11:00:00',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Assets, 1, "offline 资产必须从 Timeline 隐藏")
	require.Equal(t, "online", groups[0].Assets[0].ID)
}

// TestListAssetsExcludesOffline 验证 ListAssets 隐藏 offline=1 的资产。
func TestListAssetsExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "lao.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('online','/p1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "online", assets[0].ID)
}

// TestPersonAssetsExcludesOffline 验证人物详情页的资产列表隐藏 offline 资产。
func TestPersonAssetsExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "pao.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('online','/p1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)
	db.Exec(`INSERT INTO persons(id,name) VALUES('per1','Alice')`)
	for _, aid := range []string{"online", "offline"} {
		db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',?)`, "fd-"+aid, aid, []byte{})
		db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?,?)`, "fd-"+aid, "per1")
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.PersonAssets("per1", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "online", assets[0].ID)
}

func TestListAssets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "la.db"))
	require.NoError(t, err)
	defer db.Close()

	for i := 0; i < 3; i++ {
		db.Exec(fmt.Sprintf(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('id%d','/p%d.jpg','indexed',0)`, i, i))
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 3)
}

func TestGetAssetReturnsImageMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "img.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('img1','/tmp/x.jpg','indexed','image/jpeg')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, iso, aperture, make, focal_length, orientation)
		VALUES('img1', 4000, 3000, 800, 1.8, 'Apple', 35.0, 1)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "img1")
	require.NoError(t, err)
	require.Equal(t, 4000, a.Width)
	require.Equal(t, 3000, a.Height)
	require.Equal(t, 800, a.ISO)
	require.InDelta(t, 1.8, a.Aperture, 1e-6)
	require.Equal(t, "Apple", a.Make)
	require.InDelta(t, 35.0, a.FocalLength, 1e-6)
	require.Equal(t, 1, a.Orientation)
}

func TestGetAssetReturnsVideoMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "vid.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('vid1','/tmp/x.mp4','indexed','video/mp4')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, video_codec, audio_codec, frame_rate, bit_rate, rotation, latitude, longitude)
		VALUES('vid1', 1920, 1080, 'h264', 'aac', 29.97, 12000000, 90, 39.9, 116.4)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "vid1")
	require.NoError(t, err)
	require.Equal(t, 1920, a.Width)
	require.Equal(t, 1080, a.Height)
	require.Equal(t, "h264", a.VideoCodec)
	require.Equal(t, "aac", a.AudioCodec)
	require.InDelta(t, 29.97, a.FrameRate, 1e-3)
	require.Equal(t, int64(12000000), a.BitRate)
	require.Equal(t, 90, a.Rotation)
	require.InDelta(t, 39.9, a.Latitude, 1e-6)
	require.InDelta(t, 116.4, a.Longitude, 1e-6)
}

func TestGetAssetWithoutExifRow(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "bare.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('bare','/tmp/y.jpg','indexed')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "bare")
	require.NoError(t, err)
	require.Equal(t, "bare", a.ID)
	require.Equal(t, 0, a.Width)
}

// TestListAssetsByPlaceKey 验证 place_key 过滤只返回该城市的照片。
// TestListAssetsBySpotKey 验证 spot_key 精确过滤到网格单元内的照片。
func TestListAssetsByPlaceAndSpotKey(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "geo_filter.db"))
	require.NoError(t, err)
	defer db.Close()

	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	// seed 辅助：插入 asset + exif，并反向地理编码写入 asset_geo
	seed := func(id string, lat, lon float64) {
		_, err := db.Exec(
			`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			 VALUES(?,?,'indexed','2026-01-01 00:00:00',0)`,
			id, "/x/"+id,
		)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}

	// 东京 2 张：同网格坐标 (35.6895, 139.6917)
	seed("tok1", 35.6895, 139.6917)
	seed("tok2", 35.6895, 139.6917)
	// 纽约 1 张
	seed("nyc1", 40.71, -74.00)

	// 取东京的 city_id（通过 PlacesService.ListPlaces 查 City=="Tokyo"）
	placesSvc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := placesSvc.ListPlaces()
	require.NoError(t, err)

	var tokyoCityID int32
	for _, p := range resp.Places {
		if p.City == "Tokyo" {
			tokyoCityID = p.Key
			break
		}
	}
	require.NotZero(t, tokyoCityID, "Tokyo must appear in ListPlaces")

	searchSvc := service.NewSearchService(db, nil)

	// ── 测试 place_key 过滤 ─────────────────────────────────
	assets, err := searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: tokyoCityID})
	require.NoError(t, err)
	require.Len(t, assets, 2, "place_key filter must return only Tokyo photos")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── 测试 spot_key 过滤 ──────────────────────────────────
	// spot_key 格式：cityID:int(lat/0.01):int(lon/0.01)
	tokLat, tokLon := 35.6895, 139.6917
	gx := int(tokLat / 0.01) // 3568
	gy := int(tokLon / 0.01) // 13969
	spotKey := fmt.Sprintf("%d:%d:%d", tokyoCityID, gx, gy)

	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{SpotKey: spotKey})
	require.NoError(t, err)
	require.Len(t, assets, 2, "spot_key filter must return only the 2 Tokyo photos in that grid cell")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── 纽约 place_key 过滤 ─────────────────────────────────
	var nycCityID int32
	for _, p := range resp.Places {
		if p.City != "Tokyo" {
			nycCityID = p.Key
			break
		}
	}
	require.NotZero(t, nycCityID)
	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: nycCityID})
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "nyc1", assets[0].ID)
}

func TestUpdateDurationMs(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, duration_ms, status)
		VALUES('v1','/v/v1.mp4','video/mp4',0,'indexed')`)
	require.NoError(t, err)

	s := service.NewSearchService(db, nil) // ml 可为 nil（仅用非 CLIP 方法）
	require.NoError(t, s.UpdateDurationMs("v1", 62000))

	var got int64
	require.NoError(t, db.QueryRow(`SELECT duration_ms FROM assets WHERE id='v1'`).Scan(&got))
	require.Equal(t, int64(62000), got)
}
