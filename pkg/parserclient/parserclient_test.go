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

// TestListCaptionsFirstPageOmitsOffsetParam: offset 为空串时,GET 请求不应
// 携带 offset 查询参数(首页语义靠"缺省"表达,而非空字符串值)。
func TestListCaptionsFirstPageOmitsOffsetParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "/v1/parser/visual/captions", r.URL.Path)
		require.Equal(t, "photos", r.URL.Query().Get("source"))
		require.Equal(t, "512", r.URL.Query().Get("limit"))
		_, hasOffset := r.URL.Query()["offset"]
		require.False(t, hasOffset, "首页请求不应携带 offset 参数")
		w.Write([]byte(`{"items":[{"asset_id":"a1","text":"一只猫","mtime_ms":100}],"next_offset":"c2"}`))
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	items, next, err := c.ListCaptions(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "c2", next)
	require.Equal(t, []CaptionItem{{AssetID: "a1", Text: "一只猫", MtimeMs: 100}}, items)
}

// TestListCaptionsPassesOffsetAndNullNextBecomesEmpty: 非首页请求应携带
// offset 参数;next_offset 为 JSON null 时应转成空字符串(表示最后一页)。
func TestListCaptionsPassesOffsetAndNullNextBecomesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "c2", r.URL.Query().Get("offset"))
		w.Write([]byte(`{"items":[{"asset_id":"a2","text":"一片海","mtime_ms":200}],"next_offset":null}`))
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	items, next, err := c.ListCaptions(context.Background(), "c2")
	require.NoError(t, err)
	require.Equal(t, "", next, "next_offset 为 null 应转成空字符串,表示最后一页")
	require.Equal(t, []CaptionItem{{AssetID: "a2", Text: "一片海", MtimeMs: 200}}, items)
}

// TestListCaptionsNon2xxIsError: 非 2xx(如 503 qdrant 不可用)应返回 error,
// 调用方(Puller)据此静默跳过本轮。
func TestListCaptionsNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "parser.url"), []byte(srv.URL), 0o644))

	c := New(rt)
	_, _, err := c.ListCaptions(context.Background(), "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrParserUnavailable)
}

// TestListCaptionsDiscoveryFileMissing: Parser 未部署(discoveryFile 不存在)
// 应返回 ErrParserUnavailable,供 Puller 静默跳过整轮。
func TestListCaptionsDiscoveryFileMissing(t *testing.T) {
	c := New(t.TempDir())
	_, _, err := c.ListCaptions(context.Background(), "")
	require.ErrorIs(t, err, ErrParserUnavailable)
}
