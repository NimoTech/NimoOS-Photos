// Package parserclient 是 NimoOS-Parser visual 接口的薄客户端。
// 发现文件每次调用时读(Parser 重启换端口自适应);文件不存在返回
// ErrParserUnavailable,调用侧应静默跳过——Parser 未部署的机器不能刷日志。
package parserclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrParserUnavailable = errors.New("parser service not available")

type Client struct {
	discoveryFile string
	httpc         *http.Client
}

func New(runtimePath string) *Client {
	return &Client{
		discoveryFile: filepath.Join(runtimePath, "parser.url"),
		httpc:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) baseURL() (string, error) {
	b, err := os.ReadFile(c.discoveryFile)
	if err != nil {
		return "", ErrParserUnavailable
	}
	u := strings.TrimSpace(string(b))
	if u == "" {
		return "", ErrParserUnavailable
	}
	return u, nil
}

// IngestAsset 投喂一张资产的缩略图;takenAt/place 为空串时省略对应 meta 键。
func (c *Client) IngestAsset(ctx context.Context, assetID, imagePath, mime, takenAt, place string) error {
	base, err := c.baseURL()
	if err != nil {
		return err
	}
	meta := map[string]string{}
	if takenAt != "" {
		meta["taken_at"] = takenAt
	}
	if place != "" {
		meta["place"] = place
	}
	body, _ := json.Marshal(map[string]any{
		"source": "photos", "asset_id": assetID,
		"image_path": imagePath, "mime": mime, "meta": meta,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/parser/visual/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("parser ingest %s: HTTP %d", assetID, resp.StatusCode)
	}
	return nil
}

// DeleteAsset 删除该资产的 caption 块(Photos 删资产/入回收站时联动)。
func (c *Client) DeleteAsset(ctx context.Context, assetID string) error {
	base, err := c.baseURL()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		base+"/v1/parser/visual/assets/photos/"+assetID, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("parser delete %s: HTTP %d", assetID, resp.StatusCode)
	}
	return nil
}

// CaptionItem 是 ListCaptions 分页返回的一条 caption 记录,字段精确对齐
// Parser 端 GET /v1/parser/visual/captions 的 items 契约。
type CaptionItem struct {
	AssetID string `json:"asset_id"`
	Text    string `json:"text"`
	MtimeMs int64  `json:"mtime_ms"`
}

// captionListResponse 是 ListCaptions 端点的原始响应体。NextOffset 用指针
// 承接 JSON null(最后一页)与省略字段两种情况,统一在 ListCaptions 里折成
// 空字符串返回。
type captionListResponse struct {
	Items      []CaptionItem `json:"items"`
	NextOffset *string       `json:"next_offset"`
}

// ListCaptions 分页拉取 Parser 侧已生成的 caption(照片知识库子项目二回流侧)。
// offset 为空串表示拉首页(此时请求不携带 offset 查询参数,由 Parser 端按
// "缺省=首页"处理);返回的 nextOffset 为空串表示已是最后一页。非 2xx(含
// 503 qdrant 不可用)一律视为 error,调用方(captionpull.Puller)据此静默
// 跳过本轮,不影响 Photos 主流程。
func (c *Client) ListCaptions(ctx context.Context, offset string) ([]CaptionItem, string, error) {
	base, err := c.baseURL()
	if err != nil {
		return nil, "", err
	}
	reqURL := base + "/v1/parser/visual/captions?source=photos&limit=512"
	if offset != "" {
		reqURL += "&offset=" + url.QueryEscape(offset)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("parser list captions: HTTP %d", resp.StatusCode)
	}
	var parsed captionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, "", fmt.Errorf("parser list captions: decode: %w", err)
	}
	next := ""
	if parsed.NextOffset != nil {
		next = *parsed.NextOffset
	}
	return parsed.Items, next, nil
}
