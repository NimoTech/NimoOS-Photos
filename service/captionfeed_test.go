package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/parserclient"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// recordingSink 是供测试注入的 captionSink 假实现：记录每次调用的完整载荷，
// 并可注入 failWith 模拟投喂失败（含 ErrParserUnavailable 静默场景)。
type recordingSink struct {
	mu       sync.Mutex
	ingests  []string
	deletes  []string
	failWith error // 注入 ErrParserUnavailable / 一般错误
}

func (r *recordingSink) IngestAsset(_ context.Context, id, path, mime, takenAt, place string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.ingests = append(r.ingests, id+"|"+path+"|"+mime+"|"+takenAt+"|"+place)
	return nil
}

func (r *recordingSink) DeleteAsset(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.deletes = append(r.deletes, id)
	return nil
}

// sequenceSink 按调用顺序消费一串预设错误（nil 表示成功），用于测试需要
// 区分"第几次调用 sink"的场景（比如首次 feedInfo 失败、真正命中
// ErrParserUnavailable 的其实是第二次 sink 调用)。
type sequenceSink struct {
	mu      sync.Mutex
	results []error
	calls   []string
}

func (s *sequenceSink) IngestAsset(_ context.Context, id, _, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.calls)
	s.calls = append(s.calls, id)
	if idx < len(s.results) {
		return s.results[idx]
	}
	return nil
}

func (s *sequenceSink) DeleteAsset(context.Context, string) error { return nil }

// insertCaptionCandidate 插入一条 Backfill 会选中的资产：已索引、未软删、
// 不在离线盘上、caption_synced=0。
func insertCaptionCandidate(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status, caption_synced)
		VALUES(?,?,?,'indexed',0)`, id, "/g/"+id+".jpg", "image/jpeg")
	require.NoError(t, err)
}

// FeedOne：成功投喂后载荷含 large.jpg 路径/mime/taken_at/geo place，且置 synced=1。
func TestFeedOnePayloadAndMark(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	assetID := "a1"

	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, taken_at, status, caption_synced)
		VALUES(?,?,?,?,'indexed',0)`, assetID, "/g/a1.jpg", "image/jpeg", "2024-05-01 12:00:00")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, assetID, "Shanghai", "China")
	require.NoError(t, err)

	sink := &recordingSink{}
	f := NewCaptionFeeder(db, sink, thumbDir)
	f.FeedOne(context.Background(), assetID)

	wantPath := filepath.Join(thumbDir, assetID, "large.jpg")
	sink.mu.Lock()
	got := append([]string(nil), sink.ingests...)
	sink.mu.Unlock()
	require.Equal(t, []string{assetID + "|" + wantPath + "|image/jpeg|2024-05-01|Shanghai, China"}, got)

	var synced int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&synced))
	require.Equal(t, 1, synced)
}

// FeedOne：sink 失败时 synced 留 0；一般错误产生 Warn 日志，ErrParserUnavailable 完全静默。
func TestFeedOneFailureLeavesUnsynced(t *testing.T) {
	t.Run("一般错误留痕", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertIndexedAsset(t, db, "a1")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: errors.New("boom")}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), "a1")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.NotEmpty(t, logs.All(), "一般错误应产生日志留痕")
	})

	t.Run("ErrParserUnavailable 完全静默", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertIndexedAsset(t, db, "a1")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: parserclient.ErrParserUnavailable}
		f := NewCaptionFeeder(db, sink, thumbDir)
		f.FeedOne(context.Background(), "a1")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.Empty(t, logs.All(), "ErrParserUnavailable 不应产生任何日志")
	})
}

// Backfill：只投 synced=0 且可见的资产；成功逐个置 1；CAS 防重入；
// 首张遇 ErrParserUnavailable 时整轮静默短路。
func TestBackfillSelectionAndCAS(t *testing.T) {
	t.Run("选集正确且逐个置位", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()

		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")
		// 不入选：已同步过。
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced) VALUES('s1','/g/s1.jpg','indexed',1)`)
		require.NoError(t, err)
		// 不入选：尚未索引完。
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced) VALUES('p1','/g/p1.jpg','pending',0)`)
		require.NoError(t, err)
		// 不入选：已软删。
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced,deleted_at) VALUES('d1','/g/d1.jpg','indexed',0,CURRENT_TIMESTAMP)`)
		require.NoError(t, err)
		// 不入选：源文件在离线盘上。
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status,caption_synced,offline) VALUES('o1','/g/o1.jpg','indexed',0,1)`)
		require.NoError(t, err)

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.Backfill(context.Background()))

		sink.mu.Lock()
		var gotIDs []string
		for _, s := range sink.ingests {
			gotIDs = append(gotIDs, strings.SplitN(s, "|", 2)[0])
		}
		sink.mu.Unlock()
		require.ElementsMatch(t, []string{"e1", "e2"}, gotIDs)

		for _, id := range []string{"e1", "e2"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 1, synced, "入选资产应置 synced=1")
		}
		for _, id := range []string{"p1", "d1", "o1"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 0, synced, "不入选资产不应被动到")
		}
	})

	t.Run("首张ErrParserUnavailable整轮静默短路", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &recordingSink{failWith: parserclient.ErrParserUnavailable}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.Backfill(context.Background()))

		sink.mu.Lock()
		require.Empty(t, sink.ingests)
		sink.mu.Unlock()
		for _, id := range []string{"e1", "e2"} {
			var synced int
			require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, id).Scan(&synced))
			require.Equal(t, 0, synced)
		}
		require.Empty(t, logs.All(), "Parser 未部署时整轮应静默,不留日志")
	})

	t.Run("并发调用CAS安全", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")

		sink := &recordingSink{}
		f := NewCaptionFeeder(db, sink, thumbDir)

		done := make(chan struct{}, 2)
		for i := 0; i < 2; i++ {
			go func() {
				defer func() { done <- struct{}{} }()
				require.NoError(t, f.Backfill(context.Background()))
			}()
		}
		<-done
		<-done

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e1'`).Scan(&synced))
		require.Equal(t, 1, synced)
	})

	// 回归用例：短路判断此前死绑下标 0，若列表首条的 feedInfo 先失败（资产
	// 被并发删除等良性竞态），真正命中 ErrParserUnavailable 的是下标 1，短
	// 路会失效，整轮空遍历后仍打一条汇总 Info，违背"未部署机器零日志"诉
	// 求。用 feedBatch 直接注入一个不存在的 id 在前，模拟这种竞态。
	t.Run("首条feedInfo失败不掩盖首次sink短路", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.DebugLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &sequenceSink{results: []error{parserclient.ErrParserUnavailable}}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.feedBatch(context.Background(), []string{"ghost", "e2"}))

		sink.mu.Lock()
		gotCalls := append([]string(nil), sink.calls...)
		sink.mu.Unlock()
		require.Equal(t, []string{"e2"}, gotCalls, "sink 应只被真正调用一次（ghost 的 feedInfo 失败不算 sink 尝试）")

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e2'`).Scan(&synced))
		require.Equal(t, 0, synced)
		require.Empty(t, logs.All(), "Parser 未部署时整轮应静默，不留汇总日志")
	})

	// 非首次命中 Unavailable：Parser 曾经可用（e1 投喂成功），本轮途中才掉
	// 线，属正常运维场景，应中断循环但保留汇总日志。
	t.Run("非首次命中Unavailable保留汇总日志", func(t *testing.T) {
		db := makeTestDB(t)
		thumbDir := t.TempDir()
		insertCaptionCandidate(t, db, "e1")
		insertCaptionCandidate(t, db, "e2")

		obsCore, logs := observer.New(zap.InfoLevel)
		restore := zap.ReplaceGlobals(zap.New(obsCore))
		defer restore()

		sink := &sequenceSink{results: []error{nil, parserclient.ErrParserUnavailable}}
		f := NewCaptionFeeder(db, sink, thumbDir)
		require.NoError(t, f.feedBatch(context.Background(), []string{"e1", "e2"}))

		sink.mu.Lock()
		gotCalls := append([]string(nil), sink.calls...)
		sink.mu.Unlock()
		require.Equal(t, []string{"e1", "e2"}, gotCalls)

		var synced int
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e1'`).Scan(&synced))
		require.Equal(t, 1, synced, "e1 投喂成功应置 synced=1")
		require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='e2'`).Scan(&synced))
		require.Equal(t, 0, synced, "e2 命中 Unavailable 不应置位")

		entries := logs.All()
		require.Len(t, entries, 1, "中途掉线属正常运维场景，应保留一条汇总日志")
		require.Equal(t, "caption 补扫完成", entries[0].Message)
	})
}

// SetOnIndexed：索引流水线把资产写为 status='indexed' 后，钩子应异步被调用一次，
// 携带正确的 asset id。
func TestOnIndexedHookFires(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	ix := NewIndexer(db, &mockML{}, thumbDir, 1)

	var mu sync.Mutex
	var got []string
	done := make(chan struct{}, 1)
	ix.SetOnIndexed(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		done <- struct{}{}
	})

	srcDir := t.TempDir()
	path := makeTestJPEG(t, srcDir)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onIndexed 钩子未在超时内触发")
	}

	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&id))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{id}, got)
}
