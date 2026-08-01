package mlclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
)

func mockMLServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health check
		if r.Method == http.MethodGet && r.URL.Path == "/ping" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("pong"))
			return
		}

		// Parse multipart form
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		entries := r.FormValue("entries")

		// Build a 512-dim embedding with a known first value
		embedding := make([]float64, 512)
		embedding[0] = 0.9
		embJSON, _ := json.Marshal(embedding)
		embStr := string(embJSON) // "[0.9,0,0,...]"

		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(entries, `"clip"`) {
			_ = json.NewEncoder(w).Encode(map[string]string{"clip": embStr})
		} else if strings.Contains(entries, `"ocr"`) {
			// box is a flat array: 8 normalized floats per line (4 corner points).
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ocr": map[string]interface{}{
					"box": []float64{
						0.1, 0.1, 0.5, 0.1, 0.5, 0.2, 0.1, 0.2, // line 1: 0.4 × 0.1 rect
						0.1, 0.3, 0.3, 0.3, 0.3, 0.35, 0.1, 0.35, // line 2
					},
					"boxScore":  []float64{0.99, 0.98},
					"text":      []string{"TOTAL $42.00", "Thank you"},
					"textScore": []float64{0.97, 0.31},
				},
				"imageHeight": 100,
				"imageWidth":  100,
			})
		} else if strings.Contains(entries, `"facial-recognition"`) {
			face := map[string]interface{}{
				"boundingBox": map[string]float64{"x1": 0.1, "y1": 0.1, "x2": 0.5, "y2": 0.9},
				"embedding":   embStr,
				"score":       0.99,
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"facial-recognition": []interface{}{face},
				"imageHeight":        100,
				"imageWidth":         100,
			})
		} else {
			http.Error(w, "unknown entries", http.StatusBadRequest)
		}
	}))
}

func TestCLIPTextEmbed(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)

	vec, err := client.CLIPTextEmbed("a cat")
	if err != nil {
		t.Fatalf("CLIPTextEmbed error: %v", err)
	}
	if len(vec) != 512 {
		t.Fatalf("expected 512 dims, got %d", len(vec))
	}
	if vec[0] < 0.89 || vec[0] > 0.91 {
		t.Fatalf("expected vec[0] ≈ 0.9, got %f", vec[0])
	}
}

func TestCLIPImageEmbed(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	imageData := []byte("fake-jpeg-bytes")

	vec, err := client.CLIPImageEmbed(imageData)
	if err != nil {
		t.Fatalf("CLIPImageEmbed error: %v", err)
	}
	if len(vec) != 512 {
		t.Fatalf("expected 512 dims, got %d", len(vec))
	}
	if vec[0] < 0.89 || vec[0] > 0.91 {
		t.Fatalf("expected vec[0] ≈ 0.9, got %f", vec[0])
	}
}

func TestDetectAndRecognizeFaces(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	imageData := []byte("fake-jpeg-bytes")

	faces, err := client.DetectAndRecognizeFaces(imageData)
	if err != nil {
		t.Fatalf("DetectAndRecognizeFaces error: %v", err)
	}
	if len(faces) != 1 {
		t.Fatalf("expected 1 face, got %d", len(faces))
	}
	if faces[0].BBox.X1 < 0.09 || faces[0].BBox.X1 > 0.11 {
		t.Fatalf("expected BBox.X1 ≈ 0.1, got %f", faces[0].BBox.X1)
	}
	if len(faces[0].Embedding) != 512 {
		t.Fatalf("expected Embedding length 512, got %d", len(faces[0].Embedding))
	}
}

func TestOCR(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	lines, err := client.OCR([]byte("fake-jpeg-bytes"))
	if err != nil {
		t.Fatalf("OCR error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Text != "TOTAL $42.00" {
		t.Fatalf("expected first line 'TOTAL $42.00', got %q", lines[0].Text)
	}
	if lines[0].Score < 0.96 || lines[0].Score > 0.98 {
		t.Fatalf("expected score ≈ 0.97, got %f", lines[0].Score)
	}
	if lines[1].Text != "Thank you" || lines[1].Score > 0.32 {
		t.Fatalf("unexpected second line: %+v", lines[1])
	}
	if len(lines[0].Box) != 8 {
		t.Fatalf("expected 8 box coords, got %d", len(lines[0].Box))
	}
	if lines[0].Box[2] != 0.5 || lines[1].Box[1] != 0.3 {
		t.Fatalf("box coords mismatch: %v / %v", lines[0].Box, lines[1].Box)
	}
}

func TestIsReady(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	if !client.IsReady() {
		t.Fatal("expected IsReady() = true")
	}
}

// captureEntries starts a fake ML service, captures the received multipart
// entries field, and returns a valid response body per task type.
func captureEntries(t *testing.T, respBody string, got *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		*got = r.FormValue("entries")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestEntriesUseConfiguredModelNames(t *testing.T) {
	cases := []struct {
		name     string
		resp     string
		call     func(c *mlclient.MLClient) error
		wantSubs []string
	}{
		{
			name:     "clip visual",
			resp:     `{"clip":"[0.1]"}`,
			call:     func(c *mlclient.MLClient) error { _, err := c.CLIPImageEmbed([]byte("img")); return err },
			wantSubs: []string{`"visual"`, common.CLIPModelName},
		},
		{
			name:     "clip textual",
			resp:     `{"clip":"[0.1]"}`,
			call:     func(c *mlclient.MLClient) error { _, err := c.CLIPTextEmbed("hi"); return err },
			wantSubs: []string{`"textual"`, common.CLIPModelName},
		},
		{
			name:     "faces",
			resp:     `{"facial-recognition":[]}`,
			call:     func(c *mlclient.MLClient) error { _, err := c.DetectAndRecognizeFaces([]byte("img")); return err },
			wantSubs: []string{`"detection"`, `"recognition"`, common.FaceModelName},
		},
		{
			name:     "ocr",
			resp:     `{"ocr":{"text":[],"textScore":[],"box":[]}}`,
			call:     func(c *mlclient.MLClient) error { _, err := c.OCR([]byte("img")); return err },
			wantSubs: []string{`"ocr"`, common.OCRModelName},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var entries string
			srv := captureEntries(t, tc.resp, &entries)
			defer srv.Close()
			if err := tc.call(mlclient.New(srv.URL)); err != nil {
				t.Fatalf("call: %v", err)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(entries, sub) {
					t.Errorf("entries %q missing %q", entries, sub)
				}
			}
			// Old model names must never appear again
			for _, old := range []string{"ViT-B-32__openai", "buffalo_l", "PP-OCRv5_mobile"} {
				if strings.Contains(entries, old) {
					t.Errorf("entries %q still references old model %q", entries, old)
				}
			}
		})
	}
}
