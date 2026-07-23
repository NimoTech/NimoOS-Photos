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
