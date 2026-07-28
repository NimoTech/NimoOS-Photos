// ProfileStore 的测试:user_profile_entities 表的全量替换语义。
// 覆盖简报清单:ReplaceEntities 幂等/跨 kind 隔离、ListEntities 排序。
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── ProfileEntityID:稳定派生 id ──────────────────────────────────────────

func TestProfileEntityID(t *testing.T) {
	id1 := ProfileEntityID("pet", "beagle")
	id2 := ProfileEntityID("pet", "beagle")
	require.Equal(t, id1, id2, "同 kind+key 应得到同一 id")
	require.Len(t, id1, 16, "应为 sha1 前 16 hex")

	require.NotEqual(t, id1, ProfileEntityID("pet", "labrador"), "key 不同应换 id")
	require.NotEqual(t, id1, ProfileEntityID("person", "beagle"), "kind 不同应换 id")
}

// ── ReplaceEntities:幂等全量替换 + 跨 kind 隔离 ──────────────────────────

func TestProfileStore_ReplaceEntitiesIdempotentAndIsolatesKind(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	now := time.Now()
	pets := []ProfileEntity{
		{ID: ProfileEntityID("pet", "beagle"), Kind: "pet", Key: "beagle", Label: "Beagle", EvidenceJSON: `{"photo_count":14}`, PhotoCount: 14, FirstSeen: now.AddDate(0, -6, 0), LastSeen: now},
	}
	require.NoError(t, store.ReplaceEntities("pet", pets))

	// 跨 kind 隔离用 spec 词汇表内的 kind('pet'|'person')——family 合影集
	// 不落本表(见设计 spec 第一节:family 挖掘只对具名 person 逐一落实体行,
	// "合影集"是另一张 moments 表的产出,不经 ProfileStore),这里只是借
	// person 做隔离对照,不代表 family 引擎会调用这条路径。
	persons := []ProfileEntity{
		{ID: ProfileEntityID("person", "p1"), Kind: "person", Key: "p1", Label: "Alice", PhotoCount: 40},
	}
	require.NoError(t, store.ReplaceEntities("person", persons))

	// 跨 kind 隔离:写 person 不应影响 pet。
	petList, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList, 1)
	require.Equal(t, "beagle", petList[0].Key)

	personList, err := store.ListEntities("person")
	require.NoError(t, err)
	require.Len(t, personList, 1)
	require.Equal(t, "p1", personList[0].Key)

	// 幂等全量替换:再次写 pet(缩减为空集)应清空该 kind,不影响 person。
	require.NoError(t, store.ReplaceEntities("pet", nil))
	petList2, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList2, 0, "全量替换为空集应清空该 kind 下全部实体")

	personList2, err := store.ListEntities("person")
	require.NoError(t, err)
	require.Len(t, personList2, 1, "替换 pet 不应影响 person")

	// 再次写入不同的 pet 集合,应完全替换旧集合(而非合并)。
	require.NoError(t, store.ReplaceEntities("pet", []ProfileEntity{
		{ID: ProfileEntityID("pet", "corgi"), Kind: "pet", Key: "corgi", Label: "Corgi", PhotoCount: 20},
	}))
	petList3, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, petList3, 1)
	require.Equal(t, "corgi", petList3[0].Key)
}

// ── ListEntities:排序(按 photo_count 倒序)──────────────────────────────

func TestProfileStore_ListEntitiesOrder(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	entities := []ProfileEntity{
		{ID: ProfileEntityID("pet", "beagle"), Kind: "pet", Key: "beagle", PhotoCount: 14},
		{ID: ProfileEntityID("pet", "corgi"), Kind: "pet", Key: "corgi", PhotoCount: 40},
		{ID: ProfileEntityID("pet", "husky"), Kind: "pet", Key: "husky", PhotoCount: 8},
	}
	require.NoError(t, store.ReplaceEntities("pet", entities))

	list, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Equal(t, "corgi", list[0].Key, "photo_count 最高应排第一")
	require.Equal(t, "beagle", list[1].Key)
	require.Equal(t, "husky", list[2].Key)
}

// ── evidence/first_seen/last_seen 往返 ──────────────────────────────────

func TestProfileStore_ReplaceEntitiesRoundTripsFields(t *testing.T) {
	db := makeTestDB(t)
	store := NewProfileStore(db)

	first := time.Date(2011, 8, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.ReplaceEntities("pet", []ProfileEntity{
		{
			ID:           ProfileEntityID("pet", "beagle"),
			Kind:         "pet",
			Key:          "beagle",
			Label:        "Beagle",
			EvidenceJSON: `{"photo_count":14,"months":["2011-08","2026-07"]}`,
			PhotoCount:   14,
			FirstSeen:    first,
			LastSeen:     last,
		},
	}))

	list, err := store.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, list, 1)
	e := list[0]
	require.Equal(t, "Beagle", e.Label)
	require.Equal(t, `{"photo_count":14,"months":["2011-08","2026-07"]}`, e.EvidenceJSON)
	require.Equal(t, 14, e.PhotoCount)
	require.True(t, e.FirstSeen.Equal(first), "first_seen 应往返一致")
	require.True(t, e.LastSeen.Equal(last), "last_seen 应往返一致")
	require.Greater(t, e.UpdatedAt, int64(0), "updated_at 应由 Store 写入 Unix ms")
}
