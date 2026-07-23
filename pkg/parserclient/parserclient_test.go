// Package parserclient 的测试:用 httptest 假 Parser 服务验证薄客户端行为。
package parserclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIngestAssetPostsCorrectBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/v1/parser/visual/ingest", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(202)
		w.Write([]byte(`{"job_id":1}`))
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	err := c.IngestAsset(context.Background(), "a1", "/thumbs/a1/large.jpg", "image/jpeg", "2011-08-20", "Yosemite, US")
	require.NoError(t, err)
	require.Equal(t, "photos", got["source"])
	require.Equal(t, "a1", got["asset_id"])
	require.Equal(t, "/thumbs/a1/large.jpg", got["image_path"])
	meta := got["meta"].(map[string]any)
	require.Equal(t, "2011-08-20", meta["taken_at"])
	require.Equal(t, "Yosemite, US", meta["place"])
}

// TestMetaOmitsEmptyFields: takenAt/place 传空串时,meta 里不应出现该键。
func TestMetaOmitsEmptyFields(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(202)
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	err := c.IngestAsset(context.Background(), "a2", "/x", "image/jpeg", "", "")
	require.NoError(t, err)
	meta := got["meta"].(map[string]any)
	_, hasTakenAt := meta["taken_at"]
	_, hasPlace := meta["place"]
	require.False(t, hasTakenAt)
	require.False(t, hasPlace)
}

// TestDeleteAssetPath: DELETE /v1/parser/visual/assets/photos/a1,200 → nil。
func TestDeleteAssetPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "DELETE", r.Method)
		require.Equal(t, "/v1/parser/visual/assets/photos/a1", r.URL.Path)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	err := c.DeleteAsset(context.Background(), "a1")
	require.NoError(t, err)
}

func TestDiscoveryFileMissing(t *testing.T) {
	c := New(t.TempDir()) // 无 parser.url
	err := c.IngestAsset(context.Background(), "a1", "/x", "image/jpeg", "", "")
	require.ErrorIs(t, err, ErrParserUnavailable)
}

// TestNon2xxIsError: 假 Parser 回 400 → err != nil 且非 ErrParserUnavailable。
func TestNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	err := c.IngestAsset(context.Background(), "a1", "/x", "image/jpeg", "", "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrParserUnavailable)
}
