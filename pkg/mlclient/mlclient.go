// Package mlclient provides an HTTP client for the immich-machine-learning service.
package mlclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
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
}

// MLClient is an HTTP client for the immich-machine-learning prediction endpoint.
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

// CLIPImageEmbed returns a 512-dim CLIP embedding for the given JPEG image bytes.
func (c *MLClient) CLIPImageEmbed(imageData []byte) ([]float32, error) {
	entries := `{"clip":{"visual":{"modelName":"ViT-B-32__openai"}}}`
	body, ct := buildImageForm(entries, imageData)
	data, err := c.post(body, ct)
	if err != nil {
		return nil, err
	}
	return extractClip(data)
}

// CLIPTextEmbed returns a 512-dim CLIP embedding for the given text string.
func (c *MLClient) CLIPTextEmbed(text string) ([]float32, error) {
	entries := `{"clip":{"textual":{"modelName":"ViT-B-32__openai"}}}`
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
	entries := `{"facial-recognition":{"detection":{"modelName":"buffalo_l"},"recognition":{"modelName":"buffalo_l"}}}`
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

	// Each face: {"boundingBox":{...}, "embedding":"[...]", "score":0.9}
	var rawFaces []struct {
		BoundingBox BoundingBox `json:"boundingBox"`
		Score       float64     `json:"score"`
		Embedding   string      `json:"embedding"`
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
			BBox:      rf.BoundingBox,
			Score:     rf.Score,
			Embedding: emb,
		})
	}
	return results, nil
}

// IsReady returns true if the ml-service /ping endpoint responds with "pong".
func (c *MLClient) IsReady() bool {
	resp, err := c.http.Get(c.endpoint + "/ping")
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
