package service

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestBackfillGuard_RealCorruptSamples 用生产上真实卡住的文件验证补跑兜底。
//
// 默认跳过:样本是用户的业务数据,不入库。跑法(样本目录由 138 取回):
//
//	PHOTOS_CORRUPT_SAMPLES=/DATA/Downloads/photos-corrupt-samples-20260730 \
//	  go test ./service/ -run TestBackfillGuard_RealCorruptSamples -v
//
// 已知的三类真实故障(2026-07-30 从 192.168.1.138 取样):
//   - 0 字节的 .mp4 / .png(ffprobe 报 moov atom not found)
//   - 数据恢复产物:扩展名是 .jpg 但内容全为 0x00,没有任何 JPEG 数据
//   - macOS AppleDouble 元数据(._xxx.jpg,163 字节,magic 是 "Mac OS X")
//   - 真视频但 ffprobe 取不到时长(.m4v,duration=N/A)
//
// 断言的是本次修复真正要保证的那条不变式:**候选集会收敛**。每个样本要么补跑
// 成功拿到向量,要么被失败台账记账并退出候选集;两轮之后 queryMissing 必须为空。
// 这正是「磁盘停不下来」的止血点——旧代码里这批文件会永久留在候选集里,每轮
// 恢复链都被重新整读一遍。
//
// 跑两种 ML 桩,因为它们覆盖的是不同的失败面:
//   - mockML:对任何字节都返回合法向量。此时失败只可能来自文件本身(读不到、
//     生不出缩略图),测的是「源端永久失败」。注意这个桩会让「全 0x00 的假 jpg」
//     假性成功——它把文件字节当 fallback 喂进去就拿到向量了,真实 immich-ml 不会。
//   - decodingML:先真正解码图片,解不开就报错,近似 immich-ml/PIL 的实际行为。
//     这条覆盖「内容不是图片」的那批恢复产物在生产上的真实走向。
func TestBackfillGuard_RealCorruptSamples(t *testing.T) {
	dir := os.Getenv("PHOTOS_CORRUPT_SAMPLES")
	if dir == "" {
		t.Skip("未设置 PHOTOS_CORRUPT_SAMPLES,跳过真实样本验证")
	}

	var samples []string
	require.NoError(t, filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if supportedExts[strings.ToLower(filepath.Ext(p))] {
			samples = append(samples, p)
		}
		return nil
	}))
	require.NotEmpty(t, samples, "样本目录里没有受支持的媒体文件")

	for _, tc := range []struct {
		name string
		ml   MLProvider
	}{
		{"宽松 ML(失败只来自源文件)", &mockML{}},
		{"校验输入的 ML(近似 immich-ml)", &decodingML{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := makeTestDB(t)
			thumbDir := filepath.Join(t.TempDir(), "thumbs")

			ids := map[string]string{} // path → asset id
			for _, p := range samples {
				fi, err := os.Stat(p)
				require.NoError(t, err)
				ext := strings.ToLower(filepath.Ext(p))
				ids[p] = insertAssetWithMime(t, db, p, resolveMimeTypeByExt(ext), fi.Size())
			}

			ix := NewIndexer(db, tc.ml, thumbDir, 1)
			e := NewEmbedder(db, tc.ml, ix, NewTaskRegistry(func(Task) {}))

			// 第一轮:每条都真跑一次(该读的读、该抽帧的抽帧)。
			require.NoError(t, e.Backfill(context.Background()))
			// 第二轮紧随其后:不该再碰任何一条——成功的已有向量,失败的在冷却期内。
			require.NoError(t, e.Backfill(context.Background()))

			leftover, err := e.queryMissing(context.Background(), time.Now())
			require.NoError(t, err)

			var ok, backed int
			for _, p := range samples {
				cnt, next := readBackfillFailure(t, db, backfillCLIP, ids[p])
				if e.hasEmbeddingForPath(p) {
					ok++
					t.Logf("补出向量  %s", filepath.Base(p))
					require.Equal(t, 0, cnt, "成功的资产不该留台账: %s", p)
					continue
				}
				backed++
				t.Logf("记账退避  fail_count=%d 下次可试=%s  %s",
					cnt, time.UnixMilli(next).Format("15:04:05"), filepath.Base(p))
				require.Equal(t, 1, cnt, "永久失败的资产必须被记账一次: %s", p)
			}
			t.Logf("合计:补出向量 %d 条,记账退避 %d 条", ok, backed)

			require.Empty(t, leftover,
				"两轮之后候选集必须为空(否则下一轮恢复链又会把这些文件整读一遍)")
		})
	}
}

// decodingML 近似 immich-ml 的实际行为:输入必须是能真正解码的图片,否则报错
// (真实后端是 PIL 解不开就 500)。mockML 对任何字节都成功,会让「扩展名是 jpg
// 但内容全为 0x00」这类恢复产物假性通过。
type decodingML struct{ mockML }

func (m *decodingML) CLIPImageEmbed(d []byte) ([]float32, error) {
	if _, _, err := image.Decode(bytes.NewReader(d)); err != nil {
		return nil, fmt.Errorf("decodingML: 输入不是可解码的图片: %w", err)
	}
	return m.mockML.CLIPImageEmbed(d)
}

// insertAssetWithMime 与 insertAsset 同款,但可指定 mime 与真实文件大小。
func insertAssetWithMime(t *testing.T, db *sql.DB, path, mime string, size int64) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
        INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
                           is_live_photo_video, status, checksum)
        VALUES(?,?,?,?,?,0,'indexed',?)`,
		id, path, size, mime, filepath.Base(path), uuid.NewString())
	require.NoError(t, err)
	return id
}

// resolveMimeTypeByExt 按扩展名给出规范 MIME,与索引期的判定保持一致
// (canonicalMime 命中即用,未命中按视频/图片扩展名兜底)。
func resolveMimeTypeByExt(ext string) string {
	if m, ok := canonicalMime[ext]; ok {
		return m
	}
	if videoExts[ext] {
		return "video/mp4"
	}
	return "image/jpeg"
}
