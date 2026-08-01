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
	require.Equal(t, []string{"online"}, ids, "offline assets must not enter smart_view_matches")

	// Preview path: same condition, unpersisted — must also exclude offline.
	total, seeds, _, err := s.Preview([]string{"Sara"}, "", 50, true)
	require.NoError(t, err)
	require.Equal(t, 1, total, "the Preview count must not include offline assets")
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

	// OR syntax: matches if either receipt or invoice hits; case-insensitive
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

	// Structural condition: score is 1.0, unaffected by the threshold
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

// seedClipAssetWithSim inserts an indexed asset whose CLIP vector has cosine
// similarity exactly sim against the mock text vector ([1,0,...]) — used for
// testing score calibration.
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

// The semantic condition's threshold slider (50-99%) compares against
// SmartSearch's already-recalibrated display score (scan.go's displayScore:
// [simDisplayFloor,simDisplayCeil]→0-100%, the single calibration layer);
// evalParsed must not apply a second mapping on top of it — otherwise the
// slider's semantics would decouple from the search page's percentage.
func TestSemanticScoreCalibration(t *testing.T) {
	s := svTestService(t)
	// Seed raw scores relative to the calibration endpoints; the expected value
	// is computed live via displayScore, so this test automatically follows
	// along when the endpoints get recalibrated for a new model, instead of
	// hardcoding a percentage.
	goodRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.9 // display score 90%
	badRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.2  // display score 20%
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

// Reproduces a real user scenario: chips ["2024","bike"] — a bare year
// filters by date, bike goes through CLIP; a high-scoring 2024 photo must
// match, a 2023 one must not.
func TestBareYearPlusSemantic(t *testing.T) {
	s := svTestService(t)
	highRaw := simDisplayFloor() + (simDisplayCeil()-simDisplayFloor())*0.9 // display score 90%
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

	// The persisted score is exactly SmartSearch's display score (same scale as
	// the slider/search page percentage) — no second mapping layer is allowed
	// to rewrite it.
	var score float64
	require.NoError(t, s.db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-bike'`).Scan(&score))
	require.InDelta(t, displayScore(highRaw), score, 0.01) // persisted score = the output of the single calibration layer, displayScore
}

// Evaluate must re-parse from conds_raw rather than using the conds_parsed
// snapshot frozen at creation time: otherwise old views wouldn't self-heal
// after a parser upgrade, and relative date windows like "last 30 days"
// would also be frozen.
func TestEvaluateReparsesFromRaw(t *testing.T) {
	s := svTestService(t)
	seedClipAssetWithSim(t, s, "a24", 0.30, "2024-06-01T00:00:00Z")

	// Simulates a view created by an old version: "2024" in conds_parsed was
	// incorrectly stored as semantic
	staleParsed := `[{"raw":"2024","kind":"semantic","value":"2024"},{"raw":"bike","kind":"semantic","value":"bike"}]`
	_, err := s.db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live)
		VALUES('sv-old','Old','["2024","bike"]',?,50,1)`, staleParsed)
	require.NoError(t, err)

	require.NoError(t, s.Evaluate("sv-old"))

	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-old' AND asset_id='a24'`).Scan(&n)
	require.Equal(t, 1, n, "Evaluate should re-parse raw conds so the upgraded parser takes effect")
}

// Evaluate's reconciliation loop must never touch a manual row (origin!=0):
// a pinned row (not matching the current condition) survives and keeps a
// score of 1.0, not refreshed; an excluded row likewise survives (keeping
// its "memory" for the read path to filter); auto rows (origin=0) are added
// and removed as before — a non-matching auto row is deleted, and a newly
// matching asset is INSERTed with origin=0.
func TestEvaluatePreservesManualRows(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// aPin/aExcl/aGone all have no CLIP vector, so they naturally don't satisfy
	// the "scene: sunset" semantic condition; aMatch has an aligned CLIP vector,
	// so it naturally satisfies it (display score 1.0).
	for _, id := range []string{"aPin", "aExcl", "aGone"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
			id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	seedClipAsset(t, s, "aMatch")

	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,include_videos)
		VALUES('sv-manual','Manual','["scene: sunset"]','[]',50,0)`)
	require.NoError(t, err)

	// Pre-seed manual/stale rows in the table.
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
	require.Equal(t, 1, org, "a pinned row's origin must not be changed")
	require.Equal(t, 1.0, score, "a pinned row's score must always stay 1.0, not refreshed by a recompute")

	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aExcl'`).Scan(&org, &score))
	require.Equal(t, 2, org, "an excluded row should survive, keeping its memory")
	require.Equal(t, 0.7, score, "an excluded row's score must not be changed by a recompute")

	err = db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aGone'`).Scan(&org)
	require.ErrorIs(t, err, sql.ErrNoRows, "an auto row not matching the condition should be deleted by a recompute")

	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-manual' AND asset_id='aMatch'`).Scan(&org, &score))
	require.Equal(t, 0, org, "a newly matching asset should be auto-inserted with origin=0")
	require.Equal(t, 1.0, score)
}

// If a pinned row also naturally satisfies the condition (evalParsed would
// compute a display score <1 for it), a recompute must not refresh its
// score, and must not trigger an INSERT primary-key conflict just because
// it's already in the table.
func TestEvaluatePinnedAlsoMatching(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// Display score 50% — clearly below the pinned row's remembered 1.0, used to verify "score isn't refreshed".
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
	require.Equal(t, 1, cnt, "there should be no duplicate row/primary-key conflict")

	var org int
	var score float64
	require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-pinmatch' AND asset_id='aPin'`).Scan(&org, &score))
	require.Equal(t, 1, org)
	require.Equal(t, 1.0, score, "even if a pinned row also naturally matches, its score must not be refreshed to evalParsed's computed <1 value by a recompute")
}

// When conditions are empty (or all unactionable) but description is filled
// in, the description itself should participate in matching as a semantic
// condition — "What should Nimo match?" is no longer just decoration.
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

// When an executable condition already exists, description doesn't
// participate in matching (preserves original semantics, doesn't further
// tighten the intersection).
func TestCreateDescriptionIgnoredWhenExecutableConds(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a1','SUPERMART Receipt')`)

	// a1 has no CLIP vector — if description were incorrectly appended as a semantic condition, the intersection would be empty
	_, err := s.Create(SmartViewInput{ID: "sv-e", Name: "Receipts",
		Description: "receipts and invoices", CondsRaw: []string{"ocr: receipt"}, Threshold: 50, Live: true})
	require.NoError(t, err)

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-e' AND asset_id='a1'`).Scan(&n)
	require.Equal(t, 1, n)
}

// After Update changes description, conds_parsed should be recomputed (the fallback semantic condition follows along).
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

// Preview: description fallback + the thresholdActive flag.
func TestPreviewDescriptionFallbackAndThresholdActive(t *testing.T) {
	s := svTestService(t)
	seedClipAsset(t, s, "a1")

	count, _, thresholdActive, err := s.Preview(nil, "beach sunsets", 50, false)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.True(t, thresholdActive, "semantic fallback condition should activate the threshold slider")
}

// Preview: with a purely structural condition (OCR/person/date), thresholdActive=false — the slider has no effect.
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

// Preview should respect the includeVideos toggle, rather than hardcoding video exclusion.
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

// MatchedAssets must tolerate NULL columns (duration_ms / taken_at /
// file_size etc.): in production, an image asset's duration_ms is NULL, and
// a hand-written scan straight into int64 would error out the whole query,
// leaving all three sections of the detail page (matches/recent/activity)
// empty.
func TestMatchedAssetsTolerateNullColumns(t *testing.T) {
	s := svTestService(t)
	db := s.db
	// Insert with the minimal column set — duration_ms, taken_at, file_size, mime_type all NULL
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

// "New" tag semantics: once matched into a Smart View, an asset this user has
// never opened has isNew=true; once opened (an asset_views record at/after
// matched_at), it's permanently false.
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
	// fresh: never viewed
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at)
		VALUES('sv-v','fresh',0.9,'2026-01-01T00:00:00Z')`)
	// seen: viewed after being matched → no longer New
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,matched_at)
		VALUES('sv-v','seen',0.8,'2026-01-01T00:00:00Z')`)
	require.NoError(t, views.Record("1", "seen"))
	// rematched: the view happened before matched_at (it was re-matched in afterward) → still New
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

	// View records are per-user isolated: another user still sees "seen" as New
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

// TestFillStatsSeedsPreferAestheticScore verifies that fillStats's generated
// Seeds sort by aesthetic score first (NULL sorts last), with match_score as
// the tiebreaker within the same aesthetic tier.
func TestFillStatsSeedsPreferAestheticScore(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-aes','A','[]','[]',50)`)

	// a-null: highest match_score (0.9) but a NULL aesthetic score — should sort last
	// a-high: match_score 0.8, aesthetic score 9 — should sort first
	// a-mid:  match_score 0.7, aesthetic score 5 — should sort second
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

// TestMatchedAssetsFilterAndPinned verifies read-path filtering: an excluded
// row (origin=2) is invisible to MatchedAssets; a pinned row (origin=1) is
// visible, has Pinned=true, and sorts first with its 1.0 score.
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
	require.Len(t, assets, 2, "excluded row should be invisible")

	byID := map[string]Asset{}
	for _, a := range assets {
		byID[a.ID] = a
	}
	_, excludedPresent := byID["a-excluded"]
	require.False(t, excludedPresent, "excluded row should not appear in the result")

	require.Equal(t, "a-pinned", assets[0].ID, "a pinned row's score is always 1.0, should sort first")
	require.True(t, assets[0].Pinned, "a pinned row's Pinned should be true")
	require.False(t, byID["a-auto"].Pinned, "an auto row's Pinned should be false")
}

// TestFillStatsExcludesExcluded verifies fillStats's five stat queries
// (Count/AddedThisWeek/StorageBytes/Distribution+Median/Seeds) all exclude
// excluded rows and include pinned rows.
func TestFillStatsExcludesExcluded(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold) VALUES('sv-fs','FS','[]','[]',50)`)

	// a-auto: auto-matched, within this week
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-auto','/p/a-auto','indexed',100,8.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-auto',0.6,0,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// a-pinned: manually pinned, should count toward every stat
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-pinned','/p/a-pinned','indexed',200,9.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-pinned',1.0,1,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// a-excluded: manually excluded, should not count toward any stat
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,file_size,aesthetic_score) VALUES('a-excluded','/p/a-excluded','indexed',999999,10.0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin,matched_at)
		VALUES('sv-fs','a-excluded',0.99,2,?)`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	sv, err := s.Get("sv-fs")
	require.NoError(t, err)
	require.Equal(t, 2, sv.Count, "Count should not include the excluded row")
	require.Equal(t, 2, sv.AddedThisWeek, "AddedThisWeek should not include the excluded row")
	require.Equal(t, int64(300), sv.StorageBytes, "StorageBytes should not include the excluded row's massive byte count")
	require.Equal(t, 2, sumInts(sv.Distribution), "Distribution should not include the excluded row")
	require.NotContains(t, sv.Seeds, "a-excluded", "Seeds should not include the excluded row")
	require.Contains(t, sv.Seeds, "a-pinned", "Seeds should include the pinned row")
}

// TestExportRespectsOrigin verifies ExportAsAlbum reuses the same read-path
// filtering via MatchedAssets: a pinned row exports, an excluded row doesn't.
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
	require.True(t, ids["a-pinned"], "pinned row should export")
	require.True(t, ids["a-auto"], "auto-matched row should export")
	require.False(t, ids["a-excluded"], "excluded row should not export")
}

// TestUpdateResumeLiveTriggersEvaluate: while paused, the displayScore
// calibration endpoints (simDisplayFloor/Ceil) may have shifted with a model
// changeover, leaving match_score on the old scale. Resuming live (Live:
// true) must trigger a recompute to refresh the stale score; a patch that
// doesn't affect matching (like changing just name) should not trigger a
// recompute.
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

	// Create()'s first Evaluate has already inserted a real match (a pure
	// structural condition, score=1.0). Here we change it to a stale old-scale
	// score, simulating a leftover old match_score after the calibration
	// endpoints changed while paused.
	_, err = db.Exec(`UPDATE smart_view_matches SET match_score=0.05 WHERE smart_view_id='sv-resume' AND asset_id='a1'`)
	require.NoError(t, err)

	// A patch that doesn't affect the matching result (only changes name, Live is nil) should not trigger a recompute.
	_, err = s.Update("sv-resume", SmartViewPatch{Name: ptr("Renamed")})
	require.NoError(t, err)
	var score float64
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-resume' AND asset_id='a1'`).Scan(&score))
	require.InDelta(t, 0.05, score, 1e-9, "changing only name should not trigger a recompute")

	// Resuming live (Live: true) must recompute, refreshing the stale score back to the true value.
	_, err = s.Update("sv-resume", SmartViewPatch{Live: ptr(true)})
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-resume' AND asset_id='a1'`).Scan(&score))
	require.InDelta(t, 1.0, score, 1e-9, "resuming live should recompute and refresh the stale match_score")
}

// TestPinAssets covers PinAssets's four persistence paths + two silently-
// skipped invalid-asset cases: a new row INSERT (origin=1, score=1.0); an
// auto row (origin=0) upgraded to pinned with score changed to 1.0; an
// excluded row (origin=2) flipped to pinned; re-pinning an already-pinned row
// is idempotent and doesn't count again; a nonexistent asset / soft-deleted
// asset is silently skipped; a nonexistent view returns ErrNotFound.
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
	require.Equal(t, 3, added, "only aNew/aAuto/aExcl actually underwent a pinned-state change; aPinned is already pinned and doesn't count again; aDeleted/aMissing are silently skipped")

	for _, id := range []string{"aNew", "aAuto", "aExcl", "aPinned"} {
		var org int
		var score float64
		require.NoError(t, db.QueryRow(`SELECT origin,match_score FROM smart_view_matches WHERE smart_view_id='sv-pin' AND asset_id=?`, id).
			Scan(&org, &score), "asset %s should have a pinned row", id)
		require.Equal(t, 1, org, "asset %s should have pinned origin", id)
		require.Equal(t, 1.0, score, "asset %s's score should always be 1.0", id)
	}
	for _, id := range []string{"aDeleted", "aMissing"} {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-pin' AND asset_id=?`, id).Scan(&n))
		require.Equal(t, 0, n, "invalid asset %s should produce no row", id)
	}

	_, err = s.PinAssets("sv-missing", []string{"aNew"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRemoveAssetsTiered covers RemoveAssets's tiered removal: a pinned
// row → delete the row, counts as unpinned; an auto row → origin=2, counts
// as excluded; an already-excluded row/an id not in the table is a no-op; a
// live=1 view triggers a recompute after unpinning, and if the unpinned
// asset naturally matches the condition it comes back as origin=0; a live=0
// view doesn't trigger a recompute, so the deleted pinned row simply stays
// gone, never automatically coming back.
func TestRemoveAssetsTiered(t *testing.T) {
	s := svTestService(t)
	db := s.db

	// aPinLive has an aligned CLIP vector, so it naturally satisfies the "scene: sunset" semantic condition.
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
	require.Equal(t, 1, unpinned, "aPinLive is the only pinned row")
	require.Equal(t, 1, excluded, "aAuto is the only auto row")

	var org int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-rm-live' AND asset_id='aExcl'`).Scan(&org))
	require.Equal(t, 2, org, "an already-excluded row is a no-op, origin unchanged")

	// live=1: unpinning triggers a recompute; aPinLive naturally matches the condition, so it should come back as origin=0.
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-rm-live' AND asset_id='aPinLive'`).Scan(&org))
	require.Equal(t, 0, org, "an asset that's unpinned and naturally matches should come back as origin=0 after a recompute")

	// live=0 (paused): unpinning doesn't trigger a recompute, so the deleted row doesn't automatically come back.
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
	require.Equal(t, 0, n, "a paused view doesn't trigger a recompute, so the deleted row doesn't automatically come back")

	_, _, err = s.RemoveAssets("sv-missing", []string{"aAuto"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRestoreAssets covers RestoreAssets: deleting an excluded row counts as
// restored, a live=1 view triggers a recompute so a naturally matching asset
// comes back as origin=0; a non-excluded row (auto/pinned) is a no-op — not
// counted, not changed.
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
	require.Equal(t, 1, restored, "only aExclLive is an excluded row")

	var org int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-restore' AND asset_id='aExclLive'`).Scan(&org))
	require.Equal(t, 0, org, "after restoring, a naturally matching asset should come back as origin=0 after a recompute")

	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-restore' AND asset_id='aPin'`).Scan(&org))
	require.Equal(t, 1, org, "a pinned row is unaffected by RestoreAssets")

	_, err = s.RestoreAssets("sv-missing", []string{"aExclLive"})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestExcludedAssets only returns assets with origin=2 that are visible (not
// soft-deleted, not offline); origin=0/1 rows and soft-deleted/offline
// excluded rows should never appear.
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

// TestDuplicateDoesNotCopyManualRows: the spec makes clear Duplicate only
// copies the query definition (conditions/threshold etc.), not manual rows
// (pinned/excluded). The copy gets a brand-new smart_view_id, and since
// smart_view_matches is partitioned by smart_view_id, the original view's
// manual rows naturally don't get copied along with it; this test pins down
// that behavior, guarding against a future change (e.g. a "full clone"
// requirement) accidentally introducing copying.
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
	require.Equal(t, 0, excluded, "aPin is a pinned row; RemoveAssets turns it back to unpinned rather than excluded")
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES(?,?,0.6,2)`,
		orig.ID, "aExcl")
	require.NoError(t, err)

	dup, err := s.Duplicate(orig.ID)
	require.NoError(t, err)
	require.NotEqual(t, orig.ID, dup.ID)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id=?`, dup.ID).Scan(&n))
	require.Equal(t, 0, n, "the copy should not copy any matches row from the original view (manual or auto)")
}

// ====================== Manual ↔ Smart Album in-place conversion ======================

// TestConvertFromAlbumSuccess covers: the original album's members are fully
// locked as pinned, Evaluate synchronously triggers and pulls in new theme
// matches, the original album is deleted, and the returned object has
// live=true.
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
	// a-new matches via the face condition and isn't in the original album — verifies that the post-conversion Evaluate pulls it in.
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

	// The original album should be gone.
	_, err = albumSvc.Get(album.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// The original members are all locked as pinned (origin=1).
	for _, aid := range []string{"a-old1", "a-old2"} {
		var org int
		require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id=?`, sv.ID, aid).Scan(&org))
		require.Equal(t, 1, org, aid+" should be locked as pinned")
	}
	// Evaluate should trigger synchronously, pulling in the newly matched photo (origin=0, auto match).
	var newOrg int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id=? AND asset_id='a-new'`, sv.ID).Scan(&newOrg))
	require.Equal(t, 0, newOrg)
}

// TestConvertFromAlbumNotFound: a nonexistent album should return
// ErrNotFound, and leave no half-finished smart_views row (transactional —
// an existence assertion).
func TestConvertFromAlbumNotFound(t *testing.T) {
	s := svTestService(t)
	var before int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM smart_views`).Scan(&before))

	_, err := s.ConvertFromAlbum(ConvertFromAlbumInput{AlbumID: "missing"})
	require.ErrorIs(t, err, ErrNotFound)

	var after int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM smart_views`).Scan(&after))
	require.Equal(t, before, after, "when the album doesn't exist, no half-finished smart_views row should be left")
}

// TestConvertFromAlbumDefaultsNameFromAlbum: when name is unset, it should fall back to the album's name.
func TestConvertFromAlbumDefaultsNameFromAlbum(t *testing.T) {
	s := svTestService(t)
	albumSvc := NewAlbumService(s.db)
	album, err := albumSvc.Create("My Album")
	require.NoError(t, err)

	sv, err := s.ConvertFromAlbum(ConvertFromAlbumInput{AlbumID: album.ID})
	require.NoError(t, err)
	require.Equal(t, "My Album", sv.Name)
}

// TestConvertToAlbumSuccess covers: pinned+auto-matched members are written
// into the album sorted by score DESC, excluded members aren't carried over,
// the original smart view is deleted (cascading matches), and the write
// order is exactly score DESC (album_assets.position increments in insertion
// order, and ListAssets returns sorted by position ASC).
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
	require.True(t, ids["a-pin"], "pinned member should be solidified into the album")
	require.True(t, ids["a-auto"], "auto-matched member should be solidified into the album")
	require.False(t, ids["a-excl"], "excluded member should not be carried over")

	// Order assertion: ListAssets returns sorted by position ASC, and writes
	// increment position in score DESC order, so the sequence should be exactly
	// [a-pin(1.0), a-auto(0.6)].
	require.Len(t, assets, 2, "excluded member should not count toward the sequence")
	require.Equal(t, []string{"a-pin", "a-auto"}, []string{assets[0].ID, assets[1].ID},
		"order within the album should be score DESC")

	// The original smart view should be deleted (cascading matches).
	_, err = s.Get("sv-conv")
	require.ErrorIs(t, err, ErrNotFound)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-conv'`).Scan(&n))
	require.Equal(t, 0, n, "matches should cascade-delete along with smart_views")
}

// TestConvertToAlbumRetainsOfflineMembers verifies an offline member
// (offline=1, e.g. while an external drive is unplugged) isn't lost when
// solidified by conversion: ConvertToAlbum queries smart_view_matches
// directly, unlike MatchedAssets which filters a.offline=0 — because
// conversion deletes the source smart_view, and if it reused the read
// path's offline filter, the offline members would be permanently
// unrecoverable.
//
// The assertion queries the album_assets table directly rather than via
// AlbumService.ListAssets: ListAssets itself is a read path and likewise
// filters a.offline=0 (not showing that asset in the album detail while
// offline is expected UX) — what's being verified here is "did the write
// happen", not "is it currently visible" — as long as the row has been
// written into album_assets, once the asset's drive is remounted (offline
// reset to 0) it naturally becomes visible again, which doesn't count as
// lost; the previous bug was that this row was never written at all,
// permanently unrecoverable.
func TestConvertToAlbumRetainsOfflineMembers(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,offline) VALUES('a-offline','/mnt/ext/a.jpg','indexed',0,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,offline) VALUES('a-online','/p/a-online.jpg','indexed',0,0)`)
	require.NoError(t, err)

	_, err = s.Create(SmartViewInput{ID: "sv-off", Name: "OfflineTest", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-off','a-offline',0.9,0)`)
	_, _ = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-off','a-online',0.5,0)`)

	album, err := s.ConvertToAlbum("sv-off")
	require.NoError(t, err)

	rows, err := db.Query(`SELECT asset_id FROM album_assets WHERE album_id=?`, album.ID)
	require.NoError(t, err)
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var aid string
		require.NoError(t, rows.Scan(&aid))
		ids[aid] = true
	}
	require.True(t, ids["a-offline"], "an offline member should be solidified into album_assets by the conversion, not permanently lost")
	require.True(t, ids["a-online"], "an online member should be solidified into album_assets")
}

// TestConvertToAlbumNotFound: a nonexistent smart view should return ErrNotFound.
func TestConvertToAlbumNotFound(t *testing.T) {
	s := svTestService(t)
	_, err := s.ConvertToAlbum("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestConvertToAlbumNameConflict: a name collision follows Export's existing
// 409 semantics (ErrAlbumNameExists); on failure the original smart view
// should be preserved, leaving nothing half-finished (transactional — an
// existence assertion).
func TestConvertToAlbumNameConflict(t *testing.T) {
	s := svTestService(t)
	albumSvc := NewAlbumService(s.db)
	_, err := albumSvc.Create("Dup")
	require.NoError(t, err)
	_, err = s.Create(SmartViewInput{ID: "sv-dup", Name: "Dup", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)

	_, err = s.ConvertToAlbum("sv-dup")
	require.ErrorIs(t, err, ErrAlbumNameExists)

	// The original smart view should still exist (not deleted partway through).
	_, err = s.Get("sv-dup")
	require.NoError(t, err)
}

// TestSmartViewCreatedAtShape: a smart view's List/Get/Create responses should all carry createdAt.
func TestSmartViewCreatedAtShape(t *testing.T) {
	s := svTestService(t)
	sv, err := s.Create(SmartViewInput{ID: "sv-time", Name: "T", CondsRaw: []string{}, Threshold: 70, Live: true})
	require.NoError(t, err)
	require.False(t, sv.CreatedAt.IsZero(), "Create response should carry createdAt")

	got, err := s.Get("sv-time")
	require.NoError(t, err)
	require.False(t, got.CreatedAt.IsZero(), "Get response should carry createdAt")

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.False(t, list[0].CreatedAt.IsZero(), "List response should carry createdAt")
}
