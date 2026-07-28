// family 画像挖掘 + 实体时刻引擎测试:覆盖简报 Step 1 清单——两人各自
// 出现频次达标、其中若干张同框达标产合影集,仅 1 人同框的照片不计入合影集;
// 具名人物("Alice",达标)产 "Alice Through the Years" draft,未命名人物无
// 个人 draft;person 实体落画像表(含未命名者,label 空);hidden 人物被排除;
// 频次 < MinPersonPhotos 的人物不入实体。
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// familyTestRecipe 拼一个 kind=family 的测试 recipe。
func familyTestRecipe(topPersons, minPersonPhotos, minTogetherPersons, minAssets int) MomentRecipe {
	params := RecipeParams{
		TopPersons:         topPersons,
		MinPersonPhotos:    minPersonPhotos,
		MinTogetherPersons: minTogetherPersons,
		MinAssets:          minAssets,
	}
	b, _ := json.Marshal(params)
	return MomentRecipe{Key: "profile:family", Kind: "family", Title: "Family Entities", ParamsJSON: string(b)}
}

// insertPerson 插入一条 persons 行,name 可空(未命名),hidden 可控。
func insertPerson(t *testing.T, db *sql.DB, id, name string, hidden bool) {
	t.Helper()
	h := 0
	if hidden {
		h = 1
	}
	_, err := db.Exec(`INSERT INTO persons(id, name, hidden) VALUES(?,?,?)`, id, name, h)
	require.NoError(t, err)
}

// attachFace 给某 asset 插入一张脸并绑定到 personID,excluded 可控。
func attachFace(t *testing.T, db *sql.DB, faceID, assetID, personID string, excluded bool) {
	t.Helper()
	ex := 0
	if excluded {
		ex = 1
	}
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding, excluded) VALUES(?,?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, []byte{0}, ex)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?,?)`, faceID, personID)
	require.NoError(t, err)
}

// ── MinePersonEntities:阈值门 + hidden 排除 ──────────────────────────────

func TestMinePersonEntities_ThresholdAndHiddenExclusion(t *testing.T) {
	db := makeTestDB(t)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)

	// Alice:35 张,达标(min_person_photos=30)。
	insertPerson(t, db, "alice", "Alice", false)
	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}

	// Bob:仅 5 张,不达标。
	insertPerson(t, db, "bob", "Bob", false)
	for i := 0; i < 5; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}

	// Carol:35 张但 hidden=1,应被排除(不入实体,即使频次达标)。
	insertPerson(t, db, "carol", "Carol", true)
	for i := 0; i < 35; i++ {
		id := "carol-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "carol-face-"+itoa(i), id, "carol", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	entities, err := MinePersonEntities(context.Background(), db, recipe)
	require.NoError(t, err)

	byKey := map[string]ProfileEntity{}
	for _, e := range entities {
		byKey[e.Key] = e
	}

	require.Contains(t, byKey, "alice")
	require.Equal(t, "person", byKey["alice"].Kind)
	require.Equal(t, 35, byKey["alice"].PhotoCount)
	require.Equal(t, "Alice", byKey["alice"].Label)

	require.NotContains(t, byKey, "bob", "5 张不足以达标(min_person_photos=30)")
	require.NotContains(t, byKey, "carol", "hidden 人物应被排除")
}

// ── MinePersonEntities:excluded 人脸不计入频次 ───────────────────────────

func TestMinePersonEntities_ExcludedFaceNotCounted(t *testing.T) {
	db := makeTestDB(t)
	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "dave", "Dave", false)
	for i := 0; i < 29; i++ {
		id := "dave-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "dave-face-"+itoa(i), id, "dave", false)
	}
	// 第 30 张脸被 excluded,不应计入频次(仍差 1 张不达标)。
	insertThemeAsset(t, db, "dave-asset-excl", base.AddDate(0, 0, 30))
	attachFace(t, db, "dave-face-excl", "dave-asset-excl", "dave", true)

	recipe := familyTestRecipe(5, 30, 2, 10)
	entities, err := MinePersonEntities(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Empty(t, entities, "excluded 脸不应计入频次,29 张不足以达标")
}

// ── BuildFamilyMoments:合影集 draft ──────────────────────────────────────

func TestBuildFamilyMoments_TogetherDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.January, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "bob", "Bob", false)

	// Alice 与 Bob 各自都达标频次(35 张各自出现,含 12 张同框)。
	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 100+i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}
	// 12 张同框照片(Alice + Bob 都在场):合影集应达标(min_assets=10)。
	for i := 0; i < 12; i++ {
		id := "together-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 200+i))
		attachFace(t, db, "together-a-face-"+itoa(i), id, "alice", false)
		attachFace(t, db, "together-b-face-"+itoa(i), id, "bob", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	var together *MomentDraft
	for i := range drafts {
		if drafts[i].ID == ProfileEntityID("family", "together") {
			together = &drafts[i]
		}
	}
	require.NotNil(t, together, "同框达标应产出合影集")
	require.Equal(t, "Family Moments", together.Title)
	require.NotEmpty(t, together.Subtitle)
	require.Len(t, together.Assets, 12)
}

func TestBuildFamilyMoments_InsufficientTogetherNoDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.January, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "bob", "Bob", false)

	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "bob-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, 100+i))
		attachFace(t, db, "bob-face-"+itoa(i), id, "bob", false)
	}
	// 只有 1 张同框(< min_assets=10),合影集不应产出。
	insertThemeAsset(t, db, "together-asset-0", base.AddDate(0, 0, 200))
	attachFace(t, db, "together-a-face-0", "together-asset-0", "alice", false)
	attachFace(t, db, "together-b-face-0", "together-asset-0", "bob", false)

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	for _, d := range drafts {
		require.NotEqual(t, ProfileEntityID("family", "together"), d.ID, "同框仅 1 张不足以产出合影集")
	}
}

// ── BuildFamilyMoments:具名人物 draft ────────────────────────────────────

func TestBuildFamilyMoments_NamedPersonDraft(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2015, time.June, 1, 12, 0, 0, 0, time.UTC)

	insertPerson(t, db, "alice", "Alice", false)
	insertPerson(t, db, "unnamed1", "", false) // 未命名,频次也达标

	for i := 0; i < 35; i++ {
		id := "alice-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "alice-face-"+itoa(i), id, "alice", false)
	}
	for i := 0; i < 35; i++ {
		id := "unnamed-asset-" + itoa(i)
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		attachFace(t, db, "unnamed-face-"+itoa(i), id, "unnamed1", false)
	}

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)

	var alice *MomentDraft
	for i := range drafts {
		if drafts[i].ID == ProfileEntityID("person", "alice") {
			alice = &drafts[i]
		}
	}
	require.NotNil(t, alice, "具名达标人物应产出 Through the Years draft")
	require.Equal(t, "Alice Through the Years", alice.Title)
	require.Equal(t, 35, alice.AssetCount)

	for _, d := range drafts {
		require.NotEqual(t, ProfileEntityID("person", "unnamed1"), d.ID, "未命名人物不应产出个人 draft")
	}

	// person 实体落画像表,含未命名者(label 空)。
	saved, err := profileStore.ListEntities("person")
	require.NoError(t, err)
	byKey := map[string]ProfileEntity{}
	for _, e := range saved {
		byKey[e.Key] = e
	}
	require.Contains(t, byKey, "alice")
	require.Equal(t, "Alice", byKey["alice"].Label)
	require.Contains(t, byKey, "unnamed1", "未命名人物也应落画像表(实体挖掘不因未命名而排除)")
	require.Equal(t, "", byKey["unnamed1"].Label)
}

// ── BuildFamilyMoments:无达标实体清空画像 ────────────────────────────────

func TestBuildFamilyMoments_NoQualifyingClearsProfile(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	// 陈旧画像:模拟上一轮曾达标。
	require.NoError(t, profileStore.ReplaceEntities("person", []ProfileEntity{
		{ID: ProfileEntityID("person", "stale"), Kind: "person", Key: "stale", Label: "Stale", PhotoCount: 40},
	}))

	insertPerson(t, db, "bob", "Bob", false)
	insertThemeAsset(t, db, "bob-asset-0", time.Now())
	attachFace(t, db, "bob-face-0", "bob-asset-0", "bob", false)

	recipe := familyTestRecipe(5, 30, 2, 10)
	drafts, err := BuildFamilyMoments(context.Background(), db, profileStore, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)

	saved, err := profileStore.ListEntities("person")
	require.NoError(t, err)
	require.Empty(t, saved, "无达标实体应清空上一轮画像,不残留陈旧数据")
}

// itoa 是最小整数转字符串辅助(避免为测试引入 strconv 之外的依赖噪音;
// 这里直接用 strconv 更直白,保留此包装只是让调用点短一些)。
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
