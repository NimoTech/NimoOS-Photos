// Puller 的测试:用 fakeLister 注入分页/错误场景,验证 diff-upsert 语义。
package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"github.com/stretchr/testify/require"
)

// fakeLister 是供测试注入的 captionLister 假实现:按 offset 返回预设页,
// 或直接返回注入的错误(模拟 Parser 未部署/网络失败/503)。
type fakeLister struct {
	pages map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}
	err error
}

func (f *fakeLister) ListCaptions(_ context.Context, offset string) ([]parserclient.CaptionItem, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	p := f.pages[offset]
	return p.items, p.next, nil
}

// insertCaptionAsset 插入一条 asset_caption 会外键引用到的资产行(id 存在即可,
// 其它字段对本测试无关紧要)。
func insertCaptionAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?,'indexed')`, id, "/g/"+id+".jpg")
	require.NoError(t, err)
}

// PullOnce 应分页拉全量并逐条 upsert 进本地表。
func TestPullOnce_PagesAndUpserts(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")
	insertCaptionAsset(t, db, "a2")

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "一只猫", MtimeMs: 100}}, next: "c2"},
		"c2": {items: []parserclient.CaptionItem{{AssetID: "a2", Text: "一片海", MtimeMs: 200}}, next: ""},
	}}

	p := NewPuller(db, lister)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)

	var text string
	var mtime int64
	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "一只猫", text)
	require.Equal(t, int64(100), mtime)

	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a2'`).Scan(&text, &mtime))
	require.Equal(t, "一片海", text)
	require.Equal(t, int64(200), mtime)
}

// PullOnce 对 mtime 未变的记录跳过覆盖,对 mtime 变大的记录覆盖。
func TestPullOnce_SkipsUnchangedUpdatesChanged(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")
	_, err := db.Exec(`INSERT INTO asset_caption(asset_id, text, mtime_ms) VALUES('a1','旧文本',5)`)
	require.NoError(t, err)

	// 第一轮:mtime 相同(5),文本不同 → 不应覆盖。
	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "新文本-未变", MtimeMs: 5}}, next: ""},
	}}
	p := NewPuller(db, lister)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "mtime 未变不应计入 upsert")

	var text string
	var mtime int64
	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "旧文本", text, "mtime 未变应保留旧文本")
	require.Equal(t, int64(5), mtime)

	// 第二轮:mtime 变大(9) → 应覆盖。
	lister2 := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "a1", Text: "新文本-已变", MtimeMs: 9}}, next: ""},
	}}
	p2 := NewPuller(db, lister2)
	n2, err := p2.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n2, "mtime 变大应计入 upsert")

	require.NoError(t, db.QueryRow(`SELECT text, mtime_ms FROM asset_caption WHERE asset_id='a1'`).Scan(&text, &mtime))
	require.Equal(t, "新文本-已变", text)
	require.Equal(t, int64(9), mtime)
}

// PullOnce:lister 出错时返回 err 但不 panic,本地表不受影响(调用方挂点处仅记日志)。
func TestPullOnce_ListerErrorSilent(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a1")

	lister := &fakeLister{err: errors.New("parser 503")}
	p := NewPuller(db, lister)
	n, err := p.PullOnce(context.Background())
	require.Error(t, err)
	require.Equal(t, 0, n)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption`).Scan(&count))
	require.Equal(t, 0, count, "lister 出错时本地表不应有任何变化")
}

// PullOnce:写入时遇到"非外键"错误(用触发器模拟磁盘 I/O/约束等真实故障,
// 区别于孤儿资产的外键约束失败)应整轮直接返回 err,不能被误当孤儿静默
// continue——否则 SQLITE_BUSY 超时等真实故障会被抹平成"孤儿跳过"。
//
// 用 BEFORE INSERT 触发器对特定 asset_id 主动 RAISE(ABORT,...) 来制造一个
// "sqlite3.Error 但 ExtendedCode 不是 ErrConstraintForeignKey"的写入失败,
// 精确验证 PullOnce 是按 ExtendedCode 判断而非"任何 Exec 错误都当孤儿"。
func TestPullOnce_NonForeignKeyErrorPropagates(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "boom") // 资产真实存在,不是孤儿

	_, err := db.Exec(`
		CREATE TRIGGER trg_force_fail BEFORE INSERT ON asset_caption
		WHEN NEW.asset_id = 'boom'
		BEGIN
			SELECT RAISE(ABORT, 'forced non-fk failure for test');
		END;`)
	require.NoError(t, err)

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{{AssetID: "boom", Text: "x", MtimeMs: 1}}, next: ""},
	}}
	p := NewPuller(db, lister)
	n, err := p.PullOnce(context.Background())
	require.Error(t, err, "非外键的写入失败应向上返回 err,不应被静默吞掉")
	require.Equal(t, 0, n)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption WHERE asset_id='boom'`).Scan(&count))
	require.Equal(t, 0, count, "写入失败不应留下半成品记录")
}

// PullOnce:遇到本地 assets 不存在的孤儿 asset_id 应跳过继续,不影响其余条目入库。
func TestPullOnce_OrphanSkipped(t *testing.T) {
	db := makeTestDB(t)
	insertCaptionAsset(t, db, "a2") // 只插 a2,a1 是孤儿

	lister := &fakeLister{pages: map[string]struct {
		items []parserclient.CaptionItem
		next  string
	}{
		"": {items: []parserclient.CaptionItem{
			{AssetID: "a1", Text: "孤儿", MtimeMs: 1},
			{AssetID: "a2", Text: "正常", MtimeMs: 2},
		}, next: ""},
	}}
	p := NewPuller(db, lister)
	n, err := p.PullOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "孤儿应跳过,只有 a2 计入 upsert")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_caption WHERE asset_id='a1'`).Scan(&count))
	require.Equal(t, 0, count, "孤儿不应写入")

	var text string
	require.NoError(t, db.QueryRow(`SELECT text FROM asset_caption WHERE asset_id='a2'`).Scan(&text))
	require.Equal(t, "正常", text)
}
