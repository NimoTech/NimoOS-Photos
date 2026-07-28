// Moments M2 画像层数据层:user_profile_entities 单表的 repo。
//
// 画像实体是"用户自己的宠物/家人"的挖掘结果(区别于 M1 概念版主题时刻的
// "全库搜含宠物元素")——挖掘发生在对应 recipe(profile:pets/profile:family)
// 的引擎构建内,与重算同事务节奏,产出按 kind 全量替换(delete kind 全部 +
// insert),幂等,跨 kind 隔离(替换 pet 不影响 family,反之亦然)。
package service

import (
	"database/sql"
	"fmt"
	"time"
)

// ProfileEntity 对应 user_profile_entities 表一行。EvidenceJSON 是挖掘依据的
// JSON 快照(photo_count/months/first/last 等),供排障与后续升级读取,不
// 参与查询过滤。FirstSeen/LastSeen 为零值 time.Time 时表示该列在库中是 NULL。
type ProfileEntity struct {
	ID           string
	Kind         string // "pet" | "person"(预留 place/activity)
	Key          string // pet: 物种词 'beagle';person: person_id
	Label        string
	EvidenceJSON string
	PhotoCount   int
	FirstSeen    time.Time
	LastSeen     time.Time
	UpdatedAt    int64 // Unix ms
}

// ProfileEntityID 派生画像实体的稳定 id:hash(kind + "|" + key) 前 16 hex。
// 与 TripMomentID/ThemeMomentID 同法(见 momentstore.go hashID16),同
// kind+key 恒定映射到同一行,重算即原地刷新。
func ProfileEntityID(kind, key string) string {
	return hashID16(kind + "|" + key)
}

// ProfileStore 是 user_profile_entities 表的 repo 层,纯 SQL,无 ORM(照
// MomentStore 的风格)。
type ProfileStore struct {
	db *sql.DB
}

// NewProfileStore 构造 ProfileStore。
func NewProfileStore(db *sql.DB) *ProfileStore {
	return &ProfileStore{db: db}
}

// ReplaceEntities 是某 kind 下画像实体的幂等全量替换入口:事务内先 delete
// 该 kind 全部旧行,再 insert 本轮 entities。挖掘结果不是增量合并——每轮
// 挖掘都是对该 kind 全库重新判定达标集合,全量替换避免"曾经达标、现在不再
// 达标"的旧实体残留。entities 为空时等价于清空该 kind(如本轮全库无实体
// 达标)。不影响其它 kind 下的行(WHERE kind=? 严格隔离)。
func (s *ProfileStore) ReplaceEntities(kind string, entities []ProfileEntity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("profile: replace entities begin: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM user_profile_entities WHERE kind=?`, kind); err != nil {
		tx.Rollback()
		return fmt.Errorf("profile: clear kind %q: %w", kind, err)
	}

	now := nowMs()
	for _, e := range entities {
		if _, err := tx.Exec(`
			INSERT INTO user_profile_entities(id, kind, key, label, evidence, photo_count, first_seen, last_seen, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, kind, e.Key, e.Label, nonEmptyOr(e.EvidenceJSON, "{}"), e.PhotoCount,
			nullTimeArg(e.FirstSeen), nullTimeArg(e.LastSeen), now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("profile: insert entity %q/%q: %w", kind, e.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("profile: replace entities commit: %w", err)
	}
	return nil
}

// nonEmptyOr 在 s 为空字符串时回落到 fallback(evidence 列 NOT NULL DEFAULT
// '{}',调用方不传 EvidenceJSON 时应落成 '{}' 而非空串)。
func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ListEntities 列出某 kind 下全部画像实体,按 photo_count 倒序(最有存在感
// 的实体排前面,如 UI 展示"你的宠物们"时的默认顺序)。
func (s *ProfileStore) ListEntities(kind string) ([]ProfileEntity, error) {
	rows, err := s.db.Query(`
		SELECT id, kind, key, label, evidence, photo_count, first_seen, last_seen, updated_at
		FROM user_profile_entities WHERE kind=? ORDER BY photo_count DESC`, kind)
	if err != nil {
		return nil, fmt.Errorf("profile: list entities %q: %w", kind, err)
	}
	defer rows.Close()

	var out []ProfileEntity
	for rows.Next() {
		var e ProfileEntity
		var first, last sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &e.Label, &e.EvidenceJSON, &e.PhotoCount, &first, &last, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("profile: scan entity: %w", err)
		}
		if t := parseSQLiteTime(first); t != nil {
			e.FirstSeen = *t
		}
		if t := parseSQLiteTime(last); t != nil {
			e.LastSeen = *t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
