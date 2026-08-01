// Package parserclient is a thin client for NimoOS-Parser's visual interface.
// The discovery file is read on every call (adapting when Parser restarts on
// a new port); if the file doesn't exist, returns ErrParserUnavailable, which
// the caller should silently skip on — a machine without Parser deployed
// must not spam the logs.
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

// IngestAsset feeds in an asset's thumbnail; when takenAt/place are empty strings, the corresponding meta key is omitted.
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

// DeleteAsset deletes this asset's caption block (triggered alongside Photos deleting an asset / moving it to trash).
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

// CaptionItem is one caption record returned by ListCaptions' pagination, its
// fields exactly matching the items contract of Parser's GET
// /v1/parser/visual/captions.
type CaptionItem struct {
	AssetID string `json:"asset_id"`
	Text    string `json:"text"`
	MtimeMs int64  `json:"mtime_ms"`
}

// captionListResponse is the raw response body of the ListCaptions endpoint.
// NextOffset uses a pointer to hold both the JSON null (last page) and
// omitted-field cases, both folded into an empty string returned by ListCaptions.
type captionListResponse struct {
	Items      []CaptionItem `json:"items"`
	NextOffset *string       `json:"next_offset"`
}

// ListCaptions paginates through captions already generated on the Parser
// side (the backflow side of the photo knowledge-base sub-project).
// offset empty means fetch the first page (in this case the request carries
// no offset query parameter, handled by Parser as "absent = first page");
// a returned nextOffset of empty string means this is the last page. Any
// non-2xx (including 503 qdrant unavailable) is treated as an error, and the
// caller (captionpull.Puller) silently skips this round based on it, without
// affecting the main Photos flow.
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
