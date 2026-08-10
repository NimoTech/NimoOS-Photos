// Package mlclient provides an HTTP client for the nimoos-photos-ml-server service.
package mlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
)

// BoundingBox represents a face detection bounding box with normalised coordinates.
type BoundingBox struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// FaceResult holds the result of a single detected face.
type FaceResult struct {
	BBox      BoundingBox `json:"boundingBox"`
	Score     float64     `json:"score"`
	Embedding []float32   `json:"-"` // parsed from JSON string in response

	// Frontality and Sharpness are optional per-face quality signals emitted
	// by nimoos-photos-ml-server. They are pointers by contract: a backend
	// that doesn't emit them (e.g. rollback to immich-ml) yields nil, which
	// downstream stores as NULL and treats as quality-neutral rather than 0.
	Frontality *float64 `json:"frontality"`
	Sharpness  *float64 `json:"sharpness"`
}

// MLClient is an HTTP client for the nimoos-photos-ml-server prediction endpoint.
type MLClient struct {
	endpoint string
	http     *http.Client
}

// New creates a new MLClient targeting the given endpoint (e.g. "http://localhost:3003").
func New(endpoint string) *MLClient {
	return &MLClient{
		endpoint: endpoint,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// buildTextForm constructs a multipart/form-data body with entries and text fields.
func buildTextForm(entries, text string) ([]byte, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("entries", entries)
	_ = w.WriteField("text", text)
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// buildImageForm constructs a multipart/form-data body with entries field and image file.
func buildImageForm(entries string, imageData []byte) ([]byte, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("entries", entries)
	part, _ := w.CreateFormFile("image", "image.jpg")
	_, _ = part.Write(imageData)
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// post sends a POST request to /predict and returns the raw response bytes.
func (c *MLClient) post(body []byte, contentType string) ([]byte, error) {
	resp, err := c.http.Post(c.endpoint+"/predict", contentType, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ml post: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ml post: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ml status %d: %s", resp.StatusCode, data)
	}
	return data, nil
}

// parseEmbeddingString parses a JSON-encoded embedding string (or nested string) into []float32.
// The immich-ml API returns embeddings as a JSON string like "[0.1,0.2,...]".
func parseEmbeddingString(s string) ([]float32, error) {
	var vec []float32
	// Try direct parse: s may already be a JSON array string "[0.1,...]"
	if err := json.Unmarshal([]byte(s), &vec); err == nil {
		return vec, nil
	}
	// Try double-quoted: s might be a JSON string containing a JSON array
	var inner string
	if err := json.Unmarshal([]byte(s), &inner); err == nil {
		if err2 := json.Unmarshal([]byte(inner), &vec); err2 == nil {
			return vec, nil
		}
	}
	return nil, fmt.Errorf("failed to parse embedding")
}

// parseClipField extracts a []float32 from the raw JSON value of the "clip" field.
// Immich ML may return either a JSON array [0.1, ...] or a JSON-encoded string "[0.1, ...]".
func parseClipField(raw json.RawMessage) ([]float32, error) {
	var vec []float32
	if err := json.Unmarshal(raw, &vec); err == nil {
		return vec, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseEmbeddingString(s)
	}
	return nil, fmt.Errorf("mlclient: cannot parse clip embedding")
}

// extractClip unmarshals the /predict response and returns the clip embedding.
func extractClip(data []byte) ([]float32, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal CLIP response: %w", err)
	}
	clipRaw, ok := raw["clip"]
	if !ok {
		return nil, fmt.Errorf("mlclient: missing clip in response")
	}
	return parseClipField(clipRaw)
}

// CLIPImageEmbed returns the CLIP embedding (common.CLIPDim dims) for the given JPEG image bytes.
func (c *MLClient) CLIPImageEmbed(imageData []byte) ([]float32, error) {
	entries := fmt.Sprintf(`{"clip":{"visual":{"modelName":%q}}}`, common.CLIPModelName)
	body, ct := buildImageForm(entries, imageData)
	data, err := c.post(body, ct)
	if err != nil {
		return nil, err
	}
	return extractClip(data)
}

// CLIPTextEmbed returns the CLIP embedding (common.CLIPDim dims) for the given text string.
func (c *MLClient) CLIPTextEmbed(text string) ([]float32, error) {
	entries := fmt.Sprintf(`{"clip":{"textual":{"modelName":%q}}}`, common.CLIPModelName)
	body, ct := buildTextForm(entries, text)
	data, err := c.post(body, ct)
	if err != nil {
		return nil, err
	}
	return extractClip(data)
}

// DetectAndRecognizeFaces sends a single request that performs both face detection
// and recognition, returning a list of FaceResult with Embedding populated.
func (c *MLClient) DetectAndRecognizeFaces(imageData []byte) ([]FaceResult, error) {
	entries := fmt.Sprintf(`{"facial-recognition":{"detection":{"modelName":%q},"recognition":{"modelName":%q}}}`,
		common.FaceModelName, common.FaceModelName)
	body, ct := buildImageForm(entries, imageData)

	data, err := c.post(body, ct)
	if err != nil {
		return nil, err
	}

	// Parse outer response: {"facial-recognition": [...], "imageHeight": H, "imageWidth": W}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal facial-recognition response: %w", err)
	}
	facesRaw, ok := raw["facial-recognition"]
	if !ok {
		return nil, fmt.Errorf("mlclient: missing facial-recognition in response")
	}

	// Each face: {"boundingBox":{...}, "embedding":"[...]", "score":0.9,
	// "frontality":0.9, "sharpness":0.4}. frontality/sharpness are optional:
	// older/rolled-back backends (immich-ml) omit them entirely, in which
	// case the pointers stay nil.
	var rawFaces []struct {
		BoundingBox BoundingBox `json:"boundingBox"`
		Score       float64     `json:"score"`
		Embedding   string      `json:"embedding"`
		Frontality  *float64    `json:"frontality"`
		Sharpness   *float64    `json:"sharpness"`
	}
	if err := json.Unmarshal(facesRaw, &rawFaces); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal face list: %w", err)
	}

	results := make([]FaceResult, 0, len(rawFaces))
	for _, rf := range rawFaces {
		emb, err := parseEmbeddingString(rf.Embedding)
		if err != nil {
			// skip faces with unparseable embeddings
			continue
		}
		results = append(results, FaceResult{
			BBox:       rf.BoundingBox,
			Score:      rf.Score,
			Embedding:  emb,
			Frontality: rf.Frontality,
			Sharpness:  rf.Sharpness,
		})
	}
	return results, nil
}

// OCRLine is one recognized text line with its recognition confidence.
// Box holds the line's quadrilateral as 8 floats (x1,y1,…,x4,y4) normalized
// to [0,1] of the image dimensions; nil when the service omits geometry.
type OCRLine struct {
	Text  string    `json:"text"`
	Score float64   `json:"score"`
	Box   []float64 `json:"box,omitempty"`
}

// OCR runs text detection + recognition (PP-OCRv5) on the given image bytes
// and returns the recognized lines. An image without any text yields an
// empty (non-nil) slice.
func (c *MLClient) OCR(imageData []byte) ([]OCRLine, error) {
	entries := fmt.Sprintf(`{"ocr":{"detection":{"modelName":%q},"recognition":{"modelName":%q}}}`,
		common.OCRModelName, common.OCRModelName)
	body, ct := buildImageForm(entries, imageData)

	data, err := c.post(body, ct)
	if err != nil {
		return nil, err
	}

	// Response: {"ocr":{"box":[...],"boxScore":[...],"text":[...],"textScore":[...]},
	//            "imageHeight":H,"imageWidth":W}
	// "box" is a FLAT float array, 8 values per line (4 corner points,
	// normalized to [0,1]) — verified against PP-OCRv5 on immich-ml.
	var raw struct {
		OCR struct {
			Text      []string  `json:"text"`
			TextScore []float64 `json:"textScore"`
			Box       []float64 `json:"box"`
		} `json:"ocr"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal ocr response: %w", err)
	}

	lines := make([]OCRLine, 0, len(raw.OCR.Text))
	for i, txt := range raw.OCR.Text {
		score := 0.0
		if i < len(raw.OCR.TextScore) {
			score = raw.OCR.TextScore[i]
		}
		var box []float64
		if (i+1)*8 <= len(raw.OCR.Box) {
			box = raw.OCR.Box[i*8 : (i+1)*8]
		}
		lines = append(lines, OCRLine{Text: txt, Score: score, Box: box})
	}
	return lines, nil
}

// IsReady returns true if the ml-service /ping endpoint responds with "pong".
// Bounded to a short timeout (independent of the client's long /predict timeout)
// so callers like the /status handler never block on a hung ML backend.
func (c *MLClient) IsReady() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/ping", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "pong")
}
