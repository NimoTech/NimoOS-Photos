package mlclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestIsReady(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	if !client.IsReady() {
		t.Fatal("expected IsReady() = true")
	}
}
