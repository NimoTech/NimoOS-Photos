package service

import (
	"database/sql"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// countingML 记录 CLIPImageEmbed / OCR 的调用次数,并可让它们必定失败。
type countingML struct {
	mockML
	clipCalls atomic.Int64
	ocrCalls  atomic.Int64
	failCLIP  bool
	failOCR   bool
}

func (m *countingML) CLIPImageEmbed(d []byte) ([]float32, error) {
	m.clipCalls.Add(1)
	if m.failCLIP {
		return nil, errors.New("ml clip boom")
	}
	return make([]float32, common.CLIPDim), nil
}

func (m *countingML) OCR(d []byte) ([]mlclient.OCRLine, error) {
	m.ocrCalls.Add(1)
	if m.failOCR {
		return nil, errors.New("ml ocr boom")
	}
	return []mlclient.OCRLine{}, nil
}

// writeSmallThumb 为资产写出 small.jpg 缩略图(内容用一张真 jpeg)。
func writeSmallThumb(t *testing.T, thumbDir, assetID, srcJPEG string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, assetID), 0o755))
	b, err := os.ReadFile(srcJPEG)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, assetID, "small.jpg"), b, 0o644))
}

// TestBackfill_EmbedsFromThumbWithoutReadingSource 是本次修复的核心断言:
// CLIP 向量的唯一输入是 small.jpg 缩略图,补跑不该为了拿它去整读源文件。
// 用「源文件已不可读、缩略图齐备」来判定——修复前补跑走 ForceReprocess 重
// 管线,第一步 os.Stat 就失败、整条放弃拿不到向量;修复后只读缩略图,照样出
// 向量。生产含义:7.3T 素材盘不再被每轮补跑整盘重读一遍。
func TestBackfill_EmbedsFromThumbWithoutReadingSource(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")
	src := makeTestJPEG(t, tmp)

	id := insertAsset(t, db, src, "indexed")
	writeSmallThumb(t, thumbDir, id, src)
	require.NoError(t, os.Remove(src)) // 源文件不可读:任何读源文件的实现都会失败

	ix := NewIndexer(db, &mockML{}, thumbDir, 1)
	e := NewEmbedder(db, &mockML{}, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.hasEmbeddingForPath(src), "缩略图齐备时补跑必须只靠缩略图就补出向量")
}

// TestBackfill_FallsBackToFullPipelineWithoutThumb 验证轻路径不是把重管线
// 换掉,而是加在它前面:缩略图缺失时仍要走 ForceReprocess 重新生成缩略图,
// 否则「从未生成过缩略图」的资产会永远补不出向量。
func TestBackfill_FallsBackToFullPipelineWithoutThumb(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")
	src := makeTestJPEG(t, tmp)

	id := insertAsset(t, db, src, "indexed")
	_ = id // 故意不写缩略图

	ix := NewIndexer(db, &mockML{}, thumbDir, 1)
	e := NewEmbedder(db, &mockML{}, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.hasEmbeddingForPath(src), "无缩略图时必须回落重管线补出向量")
}

// TestQueryMissing_SkipsAssetsInCooldown 验证候选查询尊重失败台账:
// 处于冷却期的资产不再被选中,冷却到期后重新入选。
func TestQueryMissing_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	hot := insertAsset(t, db, "/hot.jpg", "indexed")
	cold := insertAsset(t, db, "/cold.jpg", "indexed")
	_ = hot

	now := time.Unix(1_700_000_000, 0)
	recordBackfillFailure(db, backfillCLIP, cold, now, errors.New("x"))

	e := NewEmbedder(db, &mockML{}, nil, nil)

	targets, err := e.queryMissing(context.Background(), now)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "/hot.jpg", targets[0].path, "冷却中的资产不该入选")

	// 冷却到期(首档 5 分钟)后必须重新入选。
	targets, err = e.queryMissing(context.Background(), now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 2)
}

// TestBackfill_RecordsFailureThenSkipsNextRound 验证退避真的止住了重试风暴:
// ML 必定失败时,第一轮补跑试一次并记账,紧随其后的第二轮完全跳过该资产
// (调用次数不再增长)。修复前两轮都会各试一次,永不收敛。
func TestBackfill_RecordsFailureThenSkipsNextRound(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")
	src := makeTestJPEG(t, tmp)

	id := insertAsset(t, db, src, "indexed")
	writeSmallThumb(t, thumbDir, id, src)

	ml := &countingML{failCLIP: true}
	ix := NewIndexer(db, ml, thumbDir, 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.Backfill(context.Background()))
	first := ml.clipCalls.Load()
	require.Equal(t, int64(1), first, "第一轮应当尝试一次")

	cnt, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 1, cnt, "失败必须记账")

	require.NoError(t, e.Backfill(context.Background()))
	require.Equal(t, first, ml.clipCalls.Load(), "冷却期内的第二轮不该再打 ML")
}

// TestQueryMissingOCR_SkipsAssetsInCooldown 验证 OCR 候选查询同样尊重台账。
func TestQueryMissingOCR_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	_ = insertAsset(t, db, "/hot.jpg", "indexed")
	cold := insertAsset(t, db, "/cold.jpg", "indexed")

	now := time.Unix(1_700_000_000, 0)
	recordBackfillFailure(db, backfillOCR, cold, now, errors.New("x"))

	e := NewEmbedder(db, &mockML{}, nil, nil)

	targets, err := e.queryMissingOCR(context.Background(), now)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "/hot.jpg", targets[0].path)

	targets, err = e.queryMissingOCR(context.Background(), now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 2)
}

// TestBackfillOCR_RecordsFailureThenSkipsNextRound 验证 OCR 补跑的退避:
// 报告里「同样 62 张 OCR 每轮全失败、每轮重读一遍原图」就是这条缺了台账。
// 注意 OCR 补跑有「首遍全失败疑似 ML 未热好 → 等一会重试一次」的既有逻辑,
// 所以第一次 BackfillOCR 会打 ML 两次(首遍 + 重试遍),记账以重试遍为准。
func TestBackfillOCR_RecordsFailureThenSkipsNextRound(t *testing.T) {
	prev := ocrBackfillRetryDelay
	ocrBackfillRetryDelay = time.Millisecond
	t.Cleanup(func() { ocrBackfillRetryDelay = prev })

	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")

	ml := &countingML{failOCR: true}
	ix := NewIndexer(db, ml, filepath.Join(tmp, "thumbs"), 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.BackfillOCR(context.Background()))
	first := ml.ocrCalls.Load()
	require.Greater(t, first, int64(0), "第一轮应当尝试")

	cnt, _ := readBackfillFailure(t, db, backfillOCR, id)
	require.Equal(t, 1, cnt, "OCR 失败必须记账(以重试遍为准,只记一次)")

	require.NoError(t, e.BackfillOCR(context.Background()))
	require.Equal(t, first, ml.ocrCalls.Load(), "冷却期内的第二轮不该再读原图打 ML")
}

// TestBackfillOCR_ClearsFailureAfterSuccess 验证 OCR 成功后清账。
func TestBackfillOCR_ClearsFailureAfterSuccess(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	src := makeTestJPEG(t, tmp)
	id := insertAsset(t, db, src, "indexed")
	recordBackfillFailure(db, backfillOCR, id, time.Now().Add(-48*time.Hour), errors.New("old"))

	ml := &countingML{}
	ix := NewIndexer(db, ml, filepath.Join(tmp, "thumbs"), 1)
	e := NewEmbedder(db, ml, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.BackfillOCR(context.Background()))
	cnt, _ := readBackfillFailure(t, db, backfillOCR, id)
	require.Equal(t, 0, cnt, "OCR 补跑成功后台账必须清空")
}

// insertVideoAsset 插一条已索引、时长已知的视频资产(sprite 补跑的候选形态)。
func insertVideoAsset(t *testing.T, db *sql.DB, path string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
        INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
                           duration_ms, is_live_photo_video, status, checksum)
        VALUES(?,?,?, 'video/mp4', ?, 5000, 0, 'indexed', ?)`,
		id, path, 1, filepath.Base(path), uuid.NewString())
	require.NoError(t, err)
	return id
}

// TestSpriteBackfillCandidates_SkipsAssetsInCooldown 验证 sprite 候选查询
// 尊重失败台账。
func TestSpriteBackfillCandidates_SkipsAssetsInCooldown(t *testing.T) {
	db := makeTestDB(t)
	_ = insertVideoAsset(t, db, "/hot.mp4")
	cold := insertVideoAsset(t, db, "/cold.mp4")

	now := time.Unix(1_700_000_000, 0)
	recordBackfillFailure(db, backfillSprite, cold, now, errors.New("x"))

	got, err := spriteBackfillCandidates(db, now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "/hot.mp4", got[0].filePath)

	got, err = spriteBackfillCandidates(db, now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Len(t, got, 2)
}

// TestBackfillSprites_RecordsFailureThenSkipsNextRound 用一个损坏的 mp4
// (正是生产上那条 moov atom not found 的形态)验证:sprite 生成失败会记账,
// 下一轮补跑不再对它跑 ffmpeg。修复前这条视频每轮补跑都被重新拉起 ffmpeg,
// 代码注释里甚至明写了「一条永久损坏的视频每轮补跑都会失败」——只把任务栏
// 报错静音了,重试本身从未停。
func TestBackfillSprites_RecordsFailureThenSkipsNextRound(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	broken := filepath.Join(tmp, "broken.mp4")
	require.NoError(t, os.WriteFile(broken, []byte("not actually an mp4"), 0o644))
	id := insertVideoAsset(t, db, broken)

	ix := NewIndexer(db, &mockML{}, filepath.Join(tmp, "thumbs"), 1)
	ix.SetTaskRegistry(NewTaskRegistry(func(Task) {}))

	ix.BackfillSprites(context.Background())
	cnt, _ := readBackfillFailure(t, db, backfillSprite, id)
	require.Equal(t, 1, cnt, "sprite 生成失败必须记账")

	ix.BackfillSprites(context.Background())
	cnt, _ = readBackfillFailure(t, db, backfillSprite, id)
	require.Equal(t, 1, cnt, "冷却期内的第二轮不该再跑 ffmpeg(计数不应增长)")
}

// TestBackfill_ClearsFailureAfterSuccess 验证成功即清账:一次瞬时失败
// (如 ML 冷加载)不会把资产留在台账里影响后续判定。
func TestBackfill_ClearsFailureAfterSuccess(t *testing.T) {
	db := makeTestDB(t)
	tmp := t.TempDir()
	thumbDir := filepath.Join(tmp, "thumbs")
	src := makeTestJPEG(t, tmp)

	id := insertAsset(t, db, src, "indexed")
	writeSmallThumb(t, thumbDir, id, src)
	// 预置一条早已过冷却期的历史失败。
	recordBackfillFailure(db, backfillCLIP, id, time.Now().Add(-48*time.Hour), errors.New("old"))

	ix := NewIndexer(db, &mockML{}, thumbDir, 1)
	e := NewEmbedder(db, &mockML{}, ix, NewTaskRegistry(func(Task) {}))

	require.NoError(t, e.Backfill(context.Background()))
	require.True(t, e.hasEmbeddingForPath(src))
	cnt, _ := readBackfillFailure(t, db, backfillCLIP, id)
	require.Equal(t, 0, cnt, "补跑成功后台账必须清空")
}
