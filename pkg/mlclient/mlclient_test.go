package mlclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
)

func mockMLServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		modelType, _ := req["modelType"].(string)
		switch modelType {
		case "clip":
			embedding := make([]float64, 512)
			embedding[0] = 0.9
			json.NewEncoder(w).Encode(map[string]interface{}{
				"imageEmbedding": embedding,
				"textEmbedding":  embedding,
			})
		case "detection":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{
						"boundingBox": map[string]float64{"x1": 0.1, "y1": 0.1, "x2": 0.5, "y2": 0.9},
						"score":       0.99,
					},
				},
			})
		case "recognition":
			embedding := make([]float64, 512)
			embedding[1] = 0.8
			json.NewEncoder(w).Encode(map[string]interface{}{"embedding": embedding})
		default:
			http.Error(w, "unknown modelType", http.StatusBadRequest)
		}
	}))
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
}

func TestDetectFaces(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	imageData := []byte("fake-jpeg-bytes")

	faces, err := client.DetectFaces(imageData)
	if err != nil {
		t.Fatalf("DetectFaces error: %v", err)
	}
	if len(faces) != 1 {
		t.Fatalf("expected 1 face, got %d", len(faces))
	}
	if faces[0].BBox.X1 < 0.09 || faces[0].BBox.X1 > 0.11 {
		t.Fatalf("expected BBox.X1 ≈ 0.1, got %f", faces[0].BBox.X1)
	}
}

func TestRecognizeFace(t *testing.T) {
	srv := mockMLServer(t)
	defer srv.Close()

	client := mlclient.New(srv.URL)
	imageData := []byte("fake-jpeg-bytes")
	bbox := mlclient.BoundingBox{X1: 0.1, Y1: 0.1, X2: 0.5, Y2: 0.9}

	vec, err := client.RecognizeFace(imageData, bbox)
	if err != nil {
		t.Fatalf("RecognizeFace error: %v", err)
	}
	if len(vec) != 512 {
		t.Fatalf("expected 512 dims, got %d", len(vec))
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
