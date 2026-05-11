// Package mlclient provides an HTTP client for the immich-machine-learning service.
package mlclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	BBox  BoundingBox `json:"boundingBox"`
	Score float64     `json:"score"`
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

// predict posts a JSON request body to /predict and returns the raw response bytes.
func (c *MLClient) predict(payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mlclient: marshal request: %w", err)
	}

	resp, err := c.http.Post(c.endpoint+"/predict", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mlclient: HTTP POST /predict: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mlclient: /predict returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mlclient: read response body: %w", err)
	}
	return data, nil
}

// encodeImage base64-encodes raw image bytes into a data-URI string.
func encodeImage(imageData []byte) string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageData)
}

// toFloat32Slice converts a []float64 JSON slice (unmarshalled as []interface{}) to []float32.
func toFloat32Slice(raw interface{}) ([]float32, error) {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("mlclient: expected array, got %T", raw)
	}
	out := make([]float32, len(arr))
	for i, v := range arr {
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("mlclient: expected float64 at index %d, got %T", i, v)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// CLIPImageEmbed returns a 512-dim CLIP embedding for the given JPEG image bytes.
func (c *MLClient) CLIPImageEmbed(imageData []byte) ([]float32, error) {
	payload := map[string]interface{}{
		"modelName": "clip-vit-b-32__openai",
		"modelType": "clip",
		"input":     encodeImage(imageData),
	}

	data, err := c.predict(payload)
	if err != nil {
		return nil, err
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal CLIP image response: %w", err)
	}

	raw, ok := resp["imageEmbedding"]
	if !ok {
		return nil, fmt.Errorf("mlclient: missing imageEmbedding in response")
	}
	return toFloat32Slice(raw)
}

// CLIPTextEmbed returns a 512-dim CLIP embedding for the given text string.
func (c *MLClient) CLIPTextEmbed(text string) ([]float32, error) {
	payload := map[string]interface{}{
		"modelName": "clip-vit-b-32__openai",
		"modelType": "clip",
		"input":     text,
	}

	data, err := c.predict(payload)
	if err != nil {
		return nil, err
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal CLIP text response: %w", err)
	}

	raw, ok := resp["textEmbedding"]
	if !ok {
		return nil, fmt.Errorf("mlclient: missing textEmbedding in response")
	}
	return toFloat32Slice(raw)
}

// DetectFaces returns the list of detected faces in the given JPEG image bytes.
func (c *MLClient) DetectFaces(imageData []byte) ([]FaceResult, error) {
	payload := map[string]interface{}{
		"modelName": "buffalo_l",
		"modelType": "detection",
		"input":     encodeImage(imageData),
	}

	data, err := c.predict(payload)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Results []FaceResult `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal detection response: %w", err)
	}
	return resp.Results, nil
}

// RecognizeFace returns a 512-dim embedding for the face at the given bounding box within the image.
func (c *MLClient) RecognizeFace(imageData []byte, bbox BoundingBox) ([]float32, error) {
	payload := map[string]interface{}{
		"modelName": "buffalo_l",
		"modelType": "recognition",
		"input":     encodeImage(imageData),
		"crop":      []float64{bbox.X1, bbox.Y1, bbox.X2, bbox.Y2},
	}

	data, err := c.predict(payload)
	if err != nil {
		return nil, err
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mlclient: unmarshal recognition response: %w", err)
	}

	raw, ok := resp["embedding"]
	if !ok {
		return nil, fmt.Errorf("mlclient: missing embedding in response")
	}
	return toFloat32Slice(raw)
}

// IsReady returns true if the ml-service health endpoint responds with HTTP 200.
func (c *MLClient) IsReady() bool {
	resp, err := c.http.Get(c.endpoint + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
