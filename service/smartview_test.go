package service

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

type mockTextMLSV struct{}

func (m *mockTextMLSV) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

func svTestService(t *testing.T) *SmartViewService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sv.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	search := NewSearchService(db, &mockTextMLSV{})
	return NewSmartViewService(db, search)
}

func TestSmartViewCRUD(t *testing.T) {
	s := svTestService(t)
	in := SmartViewInput{ID: "sv-1", Name: "Test", CondsRaw: []string{"scene: sunset"}, Threshold: 70, Live: true}
	sv, err := s.Create(in)
	require.NoError(t, err)
	require.Equal(t, "sv-1", sv.ID)
	require.Equal(t, []string{"scene: sunset"}, sv.Conds)

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

	got, err := s.Get("sv-1")
	require.NoError(t, err)
	require.Equal(t, "Test", got.Name)

	_, err = s.Update("sv-1", SmartViewPatch{Name: ptr("Renamed")})
	require.NoError(t, err)
	got, _ = s.Get("sv-1")
	require.Equal(t, "Renamed", got.Name)

	require.NoError(t, s.Delete("sv-1"))
	_, err = s.Get("sv-1")
	require.ErrorIs(t, err, ErrNotFound)
}

func ptr[T any](v T) *T { return &v }

func TestEvaluateIntersectionAndScore(t *testing.T) {
	s := svTestService(t)
	db := s.db
	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,file_size,taken_at)
			VALUES(?,?,?,0,?,?)`, id, "/p/"+id+".jpg", "indexed", 1000, "2026-01-01T00:00:00Z")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	for _, fid := range []struct{ f, a string }{{"f1", "a1"}, {"f2", "a2"}} {
		_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',X'00')`, fid.f, fid.a)
		_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?, 'p-sara')`, fid.f)
	}
	in := SmartViewInput{ID: "sv-x", Name: "Sara sunsets",
		CondsRaw: []string{"Sara", "scene: sunset"}, Threshold: 50, Live: true}
	_, err := s.Create(in)
	require.NoError(t, err)

	var ids []string
	rows, _ := db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id='sv-x' ORDER BY asset_id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []string{"a1", "a2"}, ids)

	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-x' AND match_score>0`).Scan(&cnt)
	require.Equal(t, 2, cnt)
}

// TestEvaluateAndPreviewExcludeOfflineAssets verifies that both the persisted
// Evaluate path (used by Create/Update/EvaluateAllLive) and the unpersisted
// Preview path drop matches whose asset is currently offline=1 (removable
// drive unplugged) — the smart view must hide it exactly like every other
// list surface.
func TestEvaluateAndPreviewExcludeOfflineAssets(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('online','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('offline','/media/X/a2.jpg','indexed',0)`)
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	for _, fid := range []struct{ f, a string }{{"f1", "online"}, {"f2", "offline"}} {
		_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',X'00')`, fid.f, fid.a)
		_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?, 'p-sara')`, fid.f)
	}

	// Evaluate (live) path: only the online asset should end up in smart_view_matches.
	_, err = s.Create(SmartViewInput{ID: "sv-off", Name: "Sara", CondsRaw: []string{"Sara"}, Threshold: 50, Live: true})
	require.NoError(t, err)
	var ids []string
	rows, err := db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id='sv-off'`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []string{"online"}, ids, "offline 资产不应进入 smart_view_matches")

	// Preview path: same condition, unpersisted — must also exclude offline.
	total, seeds, _, err := s.Preview([]string{"Sara"}, "", 50, true)
	require.NoError(t, err)
	require.Equal(t, 1, total, "Preview 计数不应包含 offline 资产")
	require.Equal(t, []string{"online"}, seeds)
}

func TestEvaluatePureStructuralScoreIsOne(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES('f1','a1','{}',X'00')`)
	_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES('f1','p-sara')`)
	_, err := s.Create(SmartViewInput{ID: "sv-p", Name: "Sara", CondsRaw: []string{"Sara"}, Threshold: 90, Live: true})
	require.NoError(t, err)
	var score float64
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-p' AND asset_id='a1'`).Scan(&score))
	require.Equal(t, 1.0, score)
}

func TestEvaluateOCRCondition(t *testing.T) {
	s := svTestService(t)
	db := s.db
	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a1','SUPERMART Receipt'||char(10)||'TOTAL $42.00')`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a2','Invoice #2024-001')`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a3','')`)

	// OR 语法：receipt 或 invoice 命中即匹配；大小写不敏感
	_, err := s.Create(SmartViewInput{ID: "sv-ocr", Name: "Receipts",
		CondsRaw: []string{"ocr: receipt | invoice"}, Threshold: 80, Live: true})
	require.NoError(t, err)

	var ids []string
	rows, _ := db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id='sv-ocr' ORDER BY asset_id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []string{"a1", "a2"}, ids)

	// 结构化条件：分数为 1.0，不受阈值影响
	var score float64
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches
		WHERE smart_view_id='sv-ocr' AND asset_id='a1'`).Scan(&score))
	require.Equal(t, 1.0, score)
}

// seedClipAsset inserts an indexed asset with a CLIP embedding matching the
// mock text embedding (v[0]=1.0), so any semantic query scores it 1.0.
func seedClipAsset(t *testing.T, s *SmartViewService, id string) {
	t.Helper()
	db := s.db
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
		id, "/p/"+id+".jpg")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
	vec := make([]float32, common.CLIPDim)
	vec[0] = 1.0
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

// seedClipAssetWithSim 插入一个 indexed asset，其 CLIP 向量与 mock 文本向量
// （[1,0,...]）的余弦相似度恰为 sim——用于测试分数标定。
func seedClipAssetWithSim(t *testing.T, s *SmartViewService, id string, sim float64, takenAt string) {
	t.Helper()
	db := s.db
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at) VALUES(?,?,'indexed',0,?)`,
		id, "/p/"+id+".jpg", takenAt)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
	vec := make([]float32, common.CLIPDim)
	vec[0] = float32(sim)
	vec[1] = float32(math.Sqrt(1 - sim*sim))
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

// 语义条件的阈值滑块（50-99%）比较的是 SmartSearch 已重标定的展示分
// （scan.go displayScore：[simDisplayFloor,simDisplayCeil]→0-100%，唯一标定层），
// evalParsed 不得再做第二次映射——否则滑块语义与搜索页百分比脱钩。
func TestSemanticScoreCalibration(t *testing.T) {
	s := svTestService(t)
	// 种子裸分相对标定端点取值,期望值由 displayScore 现算,换模型重标端点时
	// 本测试自动跟随,不再硬编码百分比。
	goodRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.9 // 展示分 90%
	badRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.2 // 展示分 20%
	seedClipAssetWithSim(t, s, "good", goodRaw, "2024-06-01T00:00:00Z")
	seedClipAssetWithSim(t, s, "bad", badRaw, "2024-06-01T00:00:00Z")

	count, _, _, err := s.Preview([]string{"scene: bike"}, "", 50, false)
	require.NoError(t, err)
	require.Equal(t, 1, count, "90% must pass a 50% threshold; 20% must not")

	count, _, _, err = s.Preview([]string{"scene: bike"}, "", 70, false)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	count, _, _, err = s.Preview([]string{"scene: bike"}, "", 95, false)
	require.NoError(t, err)
	require.Equal(t, 0, count, "90% must fail a 95% threshold")
}

// 用户场景还原：chips ["2024","bike"]——裸年份按日期过滤、bike 走 CLIP，
// 2024 年的高分照片必须匹配，2023 年的不匹配。
func TestBareYearPlusSemantic(t *testing.T) {
	s := svTestService(t)
	highRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.9 // 展示分 90%
	seedClipAssetWithSim(t, s, "a24", highRaw, "2024-06-01T00:00:00Z")
	seedClipAssetWithSim(t, s, "a23", highRaw, "2023-06-01T00:00:00Z")

	_, err := s.Create(SmartViewInput{ID: "sv-bike", Name: "2024 bike",
		CondsRaw: []string{"2024", "bike"}, Threshold: 50, Live: true})
	require.NoError(t, err)

	var ids []string
	rows, _ := s.db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id='sv-bike' ORDER BY asset_id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []string{"a24"}, ids)

	// 存库分数就是 SmartSearch 的展示分（与滑块/搜索页百分比同量纲），
	// 不允许再有第二层映射改写它
	var score float64
	require.NoError(t, s.db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-bike'`).Scan(&score))
	require.InDelta(t, displayScore(highRaw), score, 0.01) // 落库分 = 唯一标定层 displayScore 的输出
}

// Evaluate 必须从 conds_raw 现解析，而不是用建库时固化的 conds_parsed 快照：
// 否则解析器升级后旧视图不会自愈，"last 30 days" 这类相对日期窗也会被冻结。
func TestEvaluateReparsesFromRaw(t *testing.T) {
	s := svTestService(t)
	seedClipAssetWithSim(t, s, "a24", 0.30, "2024-06-01T00:00:00Z")

	// 模拟旧版本创建的视图：conds_parsed 里 "2024" 被错误地存成了 semantic
	staleParsed := `[{"raw":"2024","kind":"semantic","value":"2024"},{"raw":"bike","kind":"semantic","value":"bike"}]`
	_, err := s.db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-old','Old','["2024","bike"]',?,50,1)`, staleParsed)
	require.NoError(t, err)

	require.NoError(t, s.Evaluate("sv-old"))

	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-old' AND asset_id='a24'`).Scan(&n)
	require.Equal(t, 1, n, "Evaluate should re-parse raw conds so the upgraded parser takes effect")
}

// Evaluate 的调和循环不得触碰手动行（origin!=0）：钉住行（不匹配当前条件）
// 存活且分数保持 1.0 不被刷新；排除行同样存活（保留"记忆"，供读路径过滤）；
// 自动行（origin=0）该增该删照旧——不匹配的自动行被删,新匹配的资产以
// origin=0 被 INSERT。
func TestEvaluatePreservesManualRows(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// aPin/aExcl/aGone 均无 CLIP 向量,天然不满足 "scene: sunset" 语义条件；
	// aMatch 有对齐的 CLIP 向量,天然满足条件（展示分 1.0）。
	for _, id := range []string{"aPin", "aExcl", "aGone"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	seedClipAsset(t, s, "aMatch")

	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,include_videos)
		VALUES('sv-manual','Manual','["scene: sunset"]','[]',50,0)`)
	require.NoError(t, err)

	// 预置表内手动/陈旧行。
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-manual','aPin',1.0,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-manual','aExcl',0.7,2)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-manual','aGone',0.6,0)`)
	require.NoError(t, err)

	require.NoError(t, s.Evaluate("sv-manual"))

	var org int
	var score float64
	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aPin'`).Scan(&org, &score))
	require.Equal(t, 1, org, "钉住行 origin 不应被改动")
	require.Equal(t, 1.0, score, "钉住行分数应恒为 1.0,不被重估刷新")

	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aExcl'`).Scan(&org, &score))
	require.Equal(t, 2, org, "排除行应存活,保留记忆")
	require.Equal(t, 0.7, score, "排除行分数不应被重估改动")

	err = db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aGone'`).Scan(&org)
	require.ErrorIs(t, err, sql.ErrNoRows, "不匹配条件的自动行应被重估删除")

	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aMatch'`).Scan(&org, &score))
	require.Equal(t, 0, org, "新匹配的资产应以 origin=0 自动插入")
	require.Equal(t, 1.0, score)
}

// 钉住行若同时天然满足条件（evalParsed 会为它算出 <1 的展示分）,重估也不得
// 刷新其分数,也不得因为它已在表中而触发 INSERT 主键冲突。
func TestEvaluatePinnedAlsoMatching(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// 展示分 50%——明显低于钉住行记忆中的 1.0,用于验证"不刷分"。
	raw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.5
	seedClipAssetWithSim(t, s, "aPin", raw, "2024-06-01T00:00:00Z")

	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,include_videos)
		VALUES('sv-pinmatch','PinMatch','["scene: sunset"]','[]',40,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-pinmatch','aPin',1.0,1)`)
	require.NoError(t, err)

	require.NoError(t, s.Evaluate("sv-pinmatch"))

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-pinmatch'`).Scan(&cnt))
	require.Equal(t, 1, cnt, "不应出现重复行/主键冲突")

	var org int
	var score float64
	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-pinmatch' AND asset_id='aPin'`).Scan(&org, &score))
	require.Equal(t, 1, org)
	require.Equal(t, 1.0, score, "钉住行即便天然匹配,分数也不应被重估刷新为 evalParsed 算出的 <1 值")
}

// 条件为空（或全部不可执行）但填了 description 时，描述本身应作为
// semantic 条件参与匹配——"What should Nimo match?" 不再是摆设。
func TestCreateDescriptionFallbackSemantic(t *testing.T) {
	s := svTestService(t)
	seedClipAsset(t, s, "a1")

	_, err := s.Create(SmartViewInput{ID: "sv-d", Name: "Desc only",
		Description: "sunsets at the beach", CondsRaw: []string{}, Threshold: 50, Live: true})
	require.NoError(t, err)

	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-d' AND asset_id='a1'`).Scan(&n)
	require.Equal(t, 1, n, "description should drive semantic matching when no executable conditions exist")
}

// 已有可执行条件时 description 不参与匹配（保持原语义，不额外收紧交集）。
func TestCreateDescriptionIgnoredWhenExecutableConds(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a1','SUPERMART Receipt')`)

	// a1 没有 CLIP 向量——若 description 被错误地附加为 semantic 条件，交集将为空
	_, err := s.Create(SmartViewInput{ID: "sv-e", Name: "Receipts",
		Description: "receipts and invoices", CondsRaw: []string{"ocr: receipt"}, Threshold: 50, Live: true})
	require.NoError(t, err)

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-e' AND asset_id='a1'`).Scan(&n)
	require.Equal(t, 1, n)
}

// Update 改 description 后应重算 conds_parsed（兜底语义条件跟着变）。
func TestUpdateDescriptionReparses(t *testing.T) {
	s := svTestService(t)
	_, err := s.Create(SmartViewInput{ID: "sv-u", Name: "U",
		Description: "old query", CondsRaw: []string{}, Threshold: 50, Live: true})
	require.NoError(t, err)

	_, err = s.Update("sv-u", SmartViewPatch{Description: ptr("new query")})
	require.NoError(t, err)

	var parsedJSON string
	require.NoError(t, s.db.QueryRow(`SELECT conds_parsed FROM smart_views WHERE id='sv-u'`).Scan(&parsedJSON))
	require.Contains(t, parsedJSON, "new query")
	require.NotContains(t, parsedJSON, "old query")
}

// Preview：description 兜底 + thresholdActive 标志。
func TestPreviewDescriptionFallbackAndThresholdActive(t *testing.T) {
	s := svTestService(t)
	seedClipAsset(t, s, "a1")

	count, _, thresholdActive, err := s.Preview(nil, "beach sunsets", 50, false)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.True(t, thresholdActive, "semantic fallback condition should activate the threshold slider")
}

// Preview：纯结构化条件（OCR/人物/日期）时 thresholdActive=false——滑块不生效。
func TestPreviewThresholdInactiveForStructuralConds(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a1','Invoice #1')`)

	count, _, thresholdActive, err := s.Preview([]string{"ocr: invoice"}, "", 99, false)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.False(t, thresholdActive)
}

// Preview 应尊重 includeVideos 开关，而不是硬编码排除视频。
func TestPreviewRespectsIncludeVideos(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,mime_type)
		VALUES('v1','/p/v1.mp4','indexed',0,'video/mp4')`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('v1','Invoice #1')`)

	count, _, _, err := s.Preview([]string{"ocr: invoice"}, "", 50, false)
	require.NoError(t, err)
	require.Equal(t, 0, count, "videos must stay excluded when includeVideos=false")

	count, _, _, err = s.Preview([]string{"ocr: invoice"}, "", 50, true)
	require.NoError(t, err)
	require.Equal(t, 1, count, "videos must be included when includeVideos=true")
}

// MatchedAssets 必须容忍 NULL 列（duration_ms / taken_at / file_size 等）：
// 生产库的图片资产 duration_ms 为 NULL，手写 scan 直接转 int64 会整个查询报错，
// detail 页三个区块（matches/recent/activity）随之全空。
func TestMatchedAssetsTolerateNullColumns(t *testing.T) {
	s := svTestService(t)
	db := s.db
	// 最小列集插入——duration_ms、taken_at、file_size、mime_type 全部 NULL
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-n','N','[]','[]',50)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score) VALUES('sv-n','a1',0.8)`)

	assets, err := s.MatchedAssets("sv-n", 60, 0, false, "1")
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "a1", assets[0].ID)
	require.NotNil(t, assets[0].MatchScore)
	require.InDelta(t, 0.8, *assets[0].MatchScore, 1e-9)
}

// "New" 标签语义：匹配进 Smart View 后、该用户从未打开过的资产 isNew=true；
// 打开过一次（asset_views 记录在 matched_at 之后）就永久 false。
func TestMatchedAssetsIsNewFlag(t *testing.T) {
	s := svTestService(t)
	db := s.db
	views := NewViewsService(db)
	for _, id := range []string{"fresh", "seen", "rematched"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-v','V','[]','[]',50)`)
	// fresh: 从未浏览
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at)
		VALUES('sv-v','fresh',0.9,'2026-01-01T00:00:00Z')`)
	// seen: matched 之后浏览过 → 不再 New
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at)
		VALUES('sv-v','seen',0.8,'2026-01-01T00:00:00Z')`)
	require.NoError(t, views.Record("1", "seen"))
	// rematched: 浏览发生在 matched_at 之前（之后才重新匹配进来）→ 仍是 New
	require.NoError(t, views.Record("1", "rematched"))
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at)
		VALUES('sv-v','rematched',0.7,'2030-01-01T00:00:00Z')`)

	assets, err := s.MatchedAssets("sv-v", 60, 0, false, "1")
	require.NoError(t, err)
	require.Len(t, assets, 3)
	byID := map[string]bool{}
	for _, a := range assets {
		byID[a.ID] = a.IsNew
	}
	require.True(t, byID["fresh"], "never-viewed asset should be new")
	require.False(t, byID["seen"], "viewed-after-match asset should not be new")
	require.True(t, byID["rematched"], "re-matched-after-view asset should be new again")

	// 浏览记录按用户隔离：另一个用户看 seen 依然是 New
	assets, err = s.MatchedAssets("sv-v", 60, 0, false, "2")
	require.NoError(t, err)
	for _, a := range assets {
		if a.ID == "seen" {
			require.True(t, a.IsNew, "views are per-user; user 2 never viewed it")
		}
	}
}

func TestSmartViewStats(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-s','S','[]','[]',50)`)
	for i, sc := range []float64{0.10, 0.35, 0.55, 0.75, 0.95} {
		aid := "a" + string(rune('1'+i))
		_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size) VALUES(?,?,?,?)`, aid, "/p/"+aid, "indexed", int64(100*(i+1)))
		_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at) VALUES(?,?,?,?)`,
			"sv-s", aid, sc, recentOrOld(i))
	}
	sv, err := s.Get("sv-s")
	require.NoError(t, err)
	require.Equal(t, 5, sv.Count)
	require.Len(t, sv.Distribution, 10)
	require.Equal(t, sv.Count, sumInts(sv.Distribution))
	require.Equal(t, 3, sv.AddedThisWeek)
	require.Greater(t, sv.StorageBytes, int64(0))
	require.LessOrEqual(t, sv.Median, 100)
	require.Len(t, sv.Seeds, 5)
}

// TestFillStatsSeedsPreferAestheticScore 验证:fillStats 生成的 Seeds 优先按
// 美学分排序(NULL 排在最后),同美学分档次内再按 match_score 排序。
func TestFillStatsSeedsPreferAestheticScore(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-aes','A','[]','[]',50)`)

	// a-null: match_score 最高(0.9)但美学分为 NULL —— 应排最后
	// a-high: match_score 0.8,美学分 9 —— 应排第一
	// a-mid:  match_score 0.7,美学分 5 —— 应排第二
	rows := []struct {
		id    string
		score float64
		aes   any
	}{
		{"a-null", 0.9, nil},
		{"a-high", 0.8, 9.0},
		{"a-mid", 0.7, 5.0},
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES(?,?,?,?,?)`,
			r.id, "/p/"+r.id, "indexed", 100, r.aes)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at) VALUES(?,?,?,?)`,
			"sv-aes", r.id, r.score, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		require.NoError(t, err)
	}

	sv, err := s.Get("sv-aes")
	require.NoError(t, err)
	require.Equal(t, []string{"a-high", "a-mid", "a-null"}, sv.Seeds)
}

func sumInts(a []int) int {
	s := 0
	for _, v := range a {
		s += v
	}
	return s
}

func recentOrOld(i int) string {
	if i < 3 {
		return time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	return time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02T15:04:05Z")
}

func TestDuplicate(t *testing.T) {
	s := svTestService(t)
	_, err := s.Create(SmartViewInput{ID: "sv-a", Name: "Orig", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	dup, err := s.Duplicate("sv-a")
	require.NoError(t, err)
	require.NotEqual(t, "sv-a", dup.ID)
	require.Equal(t, "Orig (copy)", dup.Name)
	require.Equal(t, 0, dup.AddedThisWeek)
}

func TestActivityLog(t *testing.T) {
	s := svTestService(t)
	_, _ = s.Create(SmartViewInput{ID: "sv-b", Name: "B", CondsRaw: []string{}, Threshold: 70, Live: true})
	acts, err := s.Activity("sv-b", 10)
	require.NoError(t, err)
	require.NotEmpty(t, acts)
	require.Equal(t, "created", acts[0].EventType)
}

func TestIncrementalEvaluateRespectsPaused(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-x','Xan')`)
	_, _ = s.Create(SmartViewInput{ID: "sv-live", Name: "L", CondsRaw: []string{"Xan"}, Threshold: 50, Live: true})
	_, _ = s.Create(SmartViewInput{ID: "sv-paused", Name: "P", CondsRaw: []string{"Xan"}, Threshold: 50, Live: false})

	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a-new','/p/n.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES('fn','a-new','{}',X'00')`)
	_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES('fn','p-x')`)

	require.NoError(t, s.EvaluateAllLive())

	var liveN, pausedN int
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-live'`).Scan(&liveN)
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-paused'`).Scan(&pausedN)
	require.Equal(t, 1, liveN)
	require.Equal(t, 0, pausedN)
}

// TestMatchedAssetsFilterAndPinned 验证读路径过滤:排除行(origin=2)对
// MatchedAssets 不可见;钉住行(origin=1)可见、Pinned=true 且以 1.0 排最前。
func TestMatchedAssetsFilterAndPinned(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-p','P','[]','[]',50)`)
	for _, id := range []string{"a-auto", "a-pinned", "a-excluded"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-p','a-auto',0.6,0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-p','a-pinned',1.0,1)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-p','a-excluded',0.9,2)`)

	assets, err := s.MatchedAssets("sv-p", 60, 0, false, "")
	require.NoError(t, err)
	require.Len(t, assets, 2, "排除行不可见")

	byID := map[string]Asset{}
	for _, a := range assets {
		byID[a.ID] = a
	}
	_, excludedPresent := byID["a-excluded"]
	require.False(t, excludedPresent, "排除行不应出现在结果中")

	require.Equal(t, "a-pinned", assets[0].ID, "钉住行分数恒 1.0,应排最前")
	require.True(t, assets[0].Pinned, "钉住行 Pinned 应为 true")
	require.False(t, byID["a-auto"].Pinned, "自动行 Pinned 应为 false")
}

// TestFillStatsExcludesExcluded 验证 fillStats 五处统计查询(Count/
// AddedThisWeek/StorageBytes/Distribution+Median/Seeds)均不含排除行、
// 且包含钉住行。
func TestFillStatsExcludesExcluded(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-fs','FS','[]','[]',50)`)

	// a-auto: 自动匹配,本周内
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-auto','/p/a-auto','indexed',100,8.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-auto',0.6,0,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// a-pinned: 手动钉住,应计入所有统计
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-pinned','/p/a-pinned','indexed',200,9.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-pinned',1.0,1,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// a-excluded: 手动排除,不应计入任何统计
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-excluded','/p/a-excluded','indexed',999999,10.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-excluded',0.99,2,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	sv, err := s.Get("sv-fs")
	require.NoError(t, err)
	require.Equal(t, 2, sv.Count, "Count 不应包含排除行")
	require.Equal(t, 2, sv.AddedThisWeek, "AddedThisWeek 不应包含排除行")
	require.Equal(t, int64(300), sv.StorageBytes, "StorageBytes 不应包含排除行的巨量字节数")
	require.Equal(t, 2, sumInts(sv.Distribution), "Distribution 不应包含排除行")
	require.NotContains(t, sv.Seeds, "a-excluded", "Seeds 不应包含排除行")
	require.Contains(t, sv.Seeds, "a-pinned", "Seeds 应包含钉住行")
}

// TestExportRespectsOrigin 验证 ExportAsAlbum 经 MatchedAssets 复用同一份读路径
// 过滤:钉住行导出、排除行不导出。
func TestExportRespectsOrigin(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-exp','EXP','[]','[]',50)`)
	for _, id := range []string{"a-auto", "a-pinned", "a-excluded"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-exp','a-auto',0.6,0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-exp','a-pinned',1.0,1)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-exp','a-excluded',0.9,2)`)

	albumID, err := s.ExportAsAlbum("sv-exp")
	require.NoError(t, err)

	albumSvc := NewAlbumService(db)
	assets, err := albumSvc.ListAssets(albumID)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, a := range assets {
		ids[a.ID] = true
	}
	require.True(t, ids["a-pinned"], "钉住行应导出")
	require.True(t, ids["a-auto"], "自动匹配行应导出")
	require.False(t, ids["a-excluded"], "排除行不应导出")
}

// TestUpdateResumeLiveTriggersEvaluate: 暂停期间 displayScore 标定端点
// (simDisplayFloor/Ceil) 可能随模型换代调整，导致 match_score 停留在旧标度。
// 恢复 live（Live: true）必须触发一次重算，把陈旧分数刷新；仅改 name 之类
// 不影响匹配的 patch 不应触发重算。
func TestUpdateResumeLiveTriggersEvaluate(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES('f1','a1','{}',X'00')`)
	_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES('f1','p-sara')`)

	_, err := s.Create(SmartViewInput{ID: "sv-resume", Name: "R",
		CondsRaw: []string{"Sara"}, Threshold: 50, Live: false})
	require.NoError(t, err)

	// Create() 的首次 Evaluate 已经插入真实匹配（纯结构条件，score=1.0）。
	// 这里把它改成一个陈旧的旧标度分数，模拟暂停期间标定端点变化后残留的
	// 旧 match_score。
	_, err = db.Exec(`UPDATE smart_view_matches SET match_score=0.05 WHERE smart_view_id='sv-resume' AND asset_id='a1'`)
	require.NoError(t, err)

	// 不影响匹配结果的 patch（仅改 name，Live 为 nil）不应触发重算。
	_, err = s.Update("sv-resume", SmartViewPatch{Name: ptr("Renamed")})
	require.NoError(t, err)
	var score float64
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-resume' AND asset_id='a1'`).Scan(&score))
	require.InDelta(t, 0.05, score, 1e-9, "仅改 name 不应触发重算")

	// 恢复 live（Live: true）必须重算，把陈旧分数刷新回真实值。
	_, err = s.Update("sv-resume", SmartViewPatch{Live: ptr(true)})
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-resume' AND asset_id='a1'`).Scan(&score))
	require.InDelta(t, 1.0, score, 1e-9, "恢复 live 应重算并刷新陈旧的 match_score")
}

// TestPinAssets 覆盖 PinAssets 的四类落库路径 + 两类无效资产静默跳过：
// 新行 INSERT(origin=1,score=1.0)；自动行(origin=0)升级钉住并改分 1.0；
// 排除行(origin=2)翻转为钉住；已钉住行重复钉住幂等不计数；
// 不存在的资产 / 软删资产静默跳过；视图不存在返回 ErrNotFound。
func TestPinAssets(t *testing.T) {
	s := svTestService(t)
	db := s.db

	for _, id := range []string{"aNew", "aAuto", "aExcl", "aPinned", "aDeleted"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE assets SET deleted_at=CURRENT_TIMESTAMP WHERE id='aDeleted'`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-pin','Pin','[]','[]',70,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-pin','aAuto',0.6,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-pin','aExcl',0.6,2)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-pin','aPinned',1.0,1)`)
	require.NoError(t, err)

	added, err := s.PinAssets("sv-pin", []string{"aNew", "aAuto", "aExcl", "aPinned", "aDeleted", "aMissing"})
	require.NoError(t, err)
	require.Equal(t, 3, added, "只有 aNew/aAuto/aExcl 三个真正发生了钉住状态变化,aPinned 已是钉住不重复计数,aDeleted/aMissing 静默跳过")

	for _, id := range []string{"aNew", "aAuto", "aExcl", "aPinned"} {
		var org int
		var score float64
		require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-pin' AND asset_id=?`, id).
			Scan(&org, &score), "asset %s 应有钉住行", id)
		require.Equal(t, 1, org, "asset %s 应为钉住 origin", id)
		require.Equal(t, 1.0, score, "asset %s 分数应恒为 1.0", id)
	}
	for _, id := range []string{"aDeleted", "aMissing"} {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-pin' AND asset_id=?`, id).Scan(&n))
		require.Equal(t, 0, n, "无效资产 %s 不应产生任何行", id)
	}

	_, err = s.PinAssets("sv-missing", []string{"aNew"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRemoveAssetsTiered 覆盖 RemoveAssets 的分层移除：钉住行→删行计 unpinned；
// 自动行→origin=2 计 excluded；排除行/表外 id no-op；live=1 的视图在取消钉住后
// 触发重估,若被取消钉住的资产天然匹配条件,会以 origin=0 回归；live=0 的视图
// 不触发重估,删掉的钉住行就此消失,不会自动回归。
func TestRemoveAssetsTiered(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// aPinLive 有对齐的 CLIP 向量,天然满足 "scene: sunset" 语义条件。
	seedClipAsset(t, s, "aPinLive")
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('aAuto','/p/aAuto.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('aExcl','/p/aExcl.jpg','indexed',0)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-rm-live','RmLive','["scene: sunset"]','[]',50,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-rm-live','aPinLive',1.0,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-rm-live','aAuto',0.6,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-rm-live','aExcl',0.6,2)`)
	require.NoError(t, err)

	unpinned, excluded, err := s.RemoveAssets("sv-rm-live", []string{"aPinLive", "aAuto", "aExcl", "aOutside"})
	require.NoError(t, err)
	require.Equal(t, 1, unpinned, "aPinLive 是唯一的钉住行")
	require.Equal(t, 1, excluded, "aAuto 是唯一的自动行")

	var org int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-rm-live' AND asset_id='aExcl'`).Scan(&org))
	require.Equal(t, 2, org, "已排除行 no-op,origin 不变")

	// live=1:取消钉住后触发重估,aPinLive 天然匹配条件,应以 origin=0 回归。
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-rm-live' AND asset_id='aPinLive'`).Scan(&org))
	require.Equal(t, 0, org, "取消钉住且天然匹配的资产,重估后应以 origin=0 回归")

	// live=0(暂停):取消钉住不触发重估,删掉的行不会自动回归。
	seedClipAsset(t, s, "aPinPaused")
	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-rm-paused','RmPaused','["scene: sunset"]','[]',50,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-rm-paused','aPinPaused',1.0,1)`)
	require.NoError(t, err)

	unpinned, _, err = s.RemoveAssets("sv-rm-paused", []string{"aPinPaused"})
	require.NoError(t, err)
	require.Equal(t, 1, unpinned)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-rm-paused' AND asset_id='aPinPaused'`).Scan(&n))
	require.Equal(t, 0, n, "暂停视图不触发重估,行删除后不会自动回归")

	_, _, err = s.RemoveAssets("sv-missing", []string{"aAuto"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRestoreAssets 覆盖 RestoreAssets：删除排除行计 restored,live=1 触发重估
// 使天然匹配的资产以 origin=0 回归；非排除行(自动/钉住)no-op 不计数、不改动。
func TestRestoreAssets(t *testing.T) {
	s := svTestService(t)
	db := s.db

	seedClipAsset(t, s, "aExclLive")
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('aPin','/p/aPin.jpg','indexed',0)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-restore','Restore','["scene: sunset"]','[]',50,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-restore','aExclLive',0.6,2)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-restore','aPin',1.0,1)`)
	require.NoError(t, err)

	restored, err := s.RestoreAssets("sv-restore", []string{"aExclLive", "aPin", "aOutside"})
	require.NoError(t, err)
	require.Equal(t, 1, restored, "只有 aExclLive 是排除行")

	var org int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-restore' AND asset_id='aExclLive'`).Scan(&org))
	require.Equal(t, 0, org, "恢复后天然匹配的资产重估应以 origin=0 回归")

	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-restore' AND asset_id='aPin'`).Scan(&org))
	require.Equal(t, 1, org, "钉住行不受 RestoreAssets 影响")

	_, err = s.RestoreAssets("sv-missing", []string{"aExclLive"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestExcludedAssets 只返回 origin=2 且可见(未软删、未离线)的资产;origin=0/1
// 行以及软删/离线的排除行都不应出现。
func TestExcludedAssets(t *testing.T) {
	s := svTestService(t)
	db := s.db

	for _, id := range []string{"aExclVisible", "aExclDeleted", "aExclOffline", "aAuto", "aPin"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE assets SET deleted_at=CURRENT_TIMESTAMP WHERE id='aExclDeleted'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='aExclOffline'`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-excl','Excl','[]','[]',70,0)`)
	require.NoError(t, err)
	for _, row := range []struct {
		id     string
		origin int
	}{
		{"aExclVisible", 2}, {"aExclDeleted", 2}, {"aExclOffline", 2},
		{"aAuto", 0}, {"aPin", 1},
	} {
		_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-excl',?,0.6,?)`,
			row.id, row.origin)
		require.NoError(t, err)
	}

	out, err := s.ExcludedAssets("sv-excl")
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "aExclVisible", out[0].ID)
}

// TestDuplicateDoesNotCopyManualRows: spec 明确 Duplicate 只复制查询定义(条件/
// 阈值等),不复制手动行(钉住/排除)。副本是全新的 smart_view_id,
// smart_view_matches 按 smart_view_id 分区,原视图的手动行天然不会跟着复制；
// 本测试锁定这一行为，防止未来改动（例如"完整克隆"需求）无意间引入复制。
func TestDuplicateDoesNotCopyManualRows(t *testing.T) {
	s := svTestService(t)
	db := s.db

	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('aPin','/p/aPin.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('aExcl','/p/aExcl.jpg','indexed',0)`)
	require.NoError(t, err)

	orig, err := s.Create(SmartViewInput{ID: "sv-dup-src", Name: "DupSrc", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	require.NoError(t, err)
	added, err := s.PinAssets(orig.ID, []string{"aPin"})
	require.NoError(t, err)
	require.Equal(t, 1, added)
	_, excluded, err := s.RemoveAssets(orig.ID, []string{"aPin"})
	require.NoError(t, err)
	require.Equal(t, 0, excluded, "aPin 是钉住行,RemoveAssets 会把它变回取消钉住而非排除")
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES(?,?,0.6,2)`,
		orig.ID, "aExcl")
	require.NoError(t, err)

	dup, err := s.Duplicate(orig.ID)
	require.NoError(t, err)
	require.NotEqual(t, orig.ID, dup.ID)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id=?`, dup.ID).Scan(&n))
	require.Equal(t, 0, n, "副本不应复制原视图的任何 matches 行(手动或自动)")
}

// ====================== 手动↔智能相册原地互转 ======================

// TestConvertFromAlbumSuccess 覆盖:原相册成员全量锁定为 pin、Evaluate 同步
// 触发吸入新的主题命中、原相册被删除、返回对象 live=true。
func TestConvertFromAlbumSuccess(t *testing.T) {
	s := svTestService(t)
	db := s.db
	albumSvc := NewAlbumService(db)

	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-x','Xan')`)
	for _, id := range []string{"a-old1", "a-old2", "a-new"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	// a-new 通过人脸条件命中,不在原相册里 —— 验证转换后的 Evaluate 会把它吸进来。
	_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES('fn','a-new','{}',X'00')`)
	_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES('fn','p-x')`)

	album, err := albumSvc.Create("Trip")
	require.NoError(t, err)
	require.NoError(t, albumSvc.BatchAddAssets(album.ID, []string{"a-old1", "a-old2"}))

	sv, err := s.ConvertFromAlbum(ConvertFromAlbumInput{
		AlbumID:   album.ID,
		CondsRaw:  []string{"Xan"},
		Threshold: 50,
	})
	require.NoError(t, err)
	require.True(t, sv.Live)
	require.Equal(t, "Trip", sv.Name)

	// 原相册应已消失。
	_, err = albumSvc.Get(album.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// 原成员全部锁定为 pin(origin=1)。
	for _, aid := range []string{"a-old1", "a-old2"} {
		var org int
		require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, sv.ID, aid).Scan(&org))
		require.Equal(t, 1, org, aid+" 应被锁定为 pin")
	}
	// Evaluate 应同步触发,吸入主题命中的新照片(origin=0 自动匹配)。
	var newOrg int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id='a-new'`, sv.ID).Scan(&newOrg))
	require.Equal(t, 0, newOrg)
}

// TestConvertFromAlbumNotFound album 不存在应返回 ErrNotFound,且不留下半成品
// smart_views 行(事务性——存在性断言)。
func TestConvertFromAlbumNotFound(t *testing.T) {
	s := svTestService(t)
	var before int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM smart_views`).Scan(&before))

	_, err := s.ConvertFromAlbum(ConvertFromAlbumInput{AlbumID: "missing"})
	require.ErrorIs(t, err, ErrNotFound)

	var after int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM smart_views`).Scan(&after))
	require.Equal(t, before, after, "album 不存在时不应留下半成品 smart_views 行")
}

// TestConvertFromAlbumDefaultsNameFromAlbum name 缺省应回退用相册名。
func TestConvertFromAlbumDefaultsNameFromAlbum(t *testing.T) {
	s := svTestService(t)
	albumSvc := NewAlbumService(s.db)
	album, err := albumSvc.Create("My Album")
	require.NoError(t, err)

	sv, err := s.ConvertFromAlbum(ConvertFromAlbumInput{AlbumID: album.ID})
	require.NoError(t, err)
	require.Equal(t, "My Album", sv.Name)
}

// TestConvertToAlbumSuccess 覆盖:pinned+自动匹配成员按 score DESC 写入相册、
// excluded 不带过去、原智能相册被删除(级联 matches)。
func TestConvertToAlbumSuccess(t *testing.T) {
	s := svTestService(t)
	db := s.db
	for _, id := range []string{"a-pin", "a-auto", "a-excl"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, err := s.Create(SmartViewInput{ID: "sv-conv", Name: "Sunset", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-conv','a-pin',1.0,1)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-conv','a-auto',0.6,0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-conv','a-excl',0.9,2)`)

	albumSvc := NewAlbumService(db)
	album, err := s.ConvertToAlbum("sv-conv")
	require.NoError(t, err)
	require.Equal(t, "Sunset", album.Name)

	assets, err := albumSvc.ListAssets(album.ID)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, a := range assets {
		ids[a.ID] = true
	}
	require.True(t, ids["a-pin"], "钉住成员应固化进相册")
	require.True(t, ids["a-auto"], "自动匹配成员应固化进相册")
	require.False(t, ids["a-excl"], "排除成员不应带过去")

	// 原智能相册应已删除(级联 matches)。
	_, err = s.Get("sv-conv")
	require.ErrorIs(t, err, ErrNotFound)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-conv'`).Scan(&n))
	require.Equal(t, 0, n, "matches 应随 smart_views 级联删除")
}

// TestConvertToAlbumNotFound smartview 不存在应返回 ErrNotFound。
func TestConvertToAlbumNotFound(t *testing.T) {
	s := svTestService(t)
	_, err := s.ConvertToAlbum("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestConvertToAlbumNameConflict 同名冲突照 Export 现有 409 语义
// (ErrAlbumNameExists);失败时原智能相册应保留、不留半成品(事务性——存在
// 性断言)。
func TestConvertToAlbumNameConflict(t *testing.T) {
	s := svTestService(t)
	albumSvc := NewAlbumService(s.db)
	_, err := albumSvc.Create("Dup")
	require.NoError(t, err)
	_, err = s.Create(SmartViewInput{ID: "sv-dup", Name: "Dup", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)

	_, err = s.ConvertToAlbum("sv-dup")
	require.ErrorIs(t, err, ErrAlbumNameExists)

	// 原智能相册仍应存在(未被半途删除)。
	_, err = s.Get("sv-dup")
	require.NoError(t, err)
}

// TestSmartViewCreatedAtShape SV 的 List/Get/Create 响应都应携带 createdAt。
func TestSmartViewCreatedAtShape(t *testing.T) {
	s := svTestService(t)
	sv, err := s.Create(SmartViewInput{ID: "sv-time", Name: "T", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	require.False(t, sv.CreatedAt.IsZero(), "Create 响应应带 createdAt")

	got, err := s.Get("sv-time")
	require.NoError(t, err)
	require.False(t, got.CreatedAt.IsZero(), "Get 响应应带 createdAt")

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].CreatedAt.IsZero(), "List 响应应带 createdAt")
}
