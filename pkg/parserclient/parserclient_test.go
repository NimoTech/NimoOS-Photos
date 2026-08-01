// Tests for package parserclient: uses an httptest fake Parser service to verify the thin client's behavior.
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

// TestMetaOmitsEmptyFields: when takenAt/place are passed empty strings, that key should not appear in meta.
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

// TestDeleteAssetPath: DELETE /v1/parser/visual/assets/photos/a1, 200 -> nil.
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
	c := New(t.TempDir()) // no parser.url
	err := c.IngestAsset(context.Background(), "a1", "/x", "image/jpeg", "", "")
	require.ErrorIs(t, err, ErrParserUnavailable)
}

// TestNon2xxIsError: fake Parser returns 400 -> err != nil and not ErrParserUnavailable.
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

// TestListCaptionsFirstPageOmitsOffsetParam: when offset is an empty string,
// the GET request should not carry an offset query parameter (first-page
// semantics are expressed via "absent," not an empty string value).
func TestListCaptionsFirstPageOmitsOffsetParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "/v1/parser/visual/captions", r.URL.Path)
		require.Equal(t, "photos", r.URL.Query().Get("source"))
		require.Equal(t, "512", r.URL.Query().Get("limit"))
		_, hasOffset := r.URL.Query()["offset"]
		require.False(t, hasOffset, "the first-page request should not carry an offset parameter")
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

// TestListCaptionsPassesOffsetAndNullNextBecomesEmpty: a non-first-page
// request should carry the offset parameter; a JSON null next_offset should
// convert to an empty string (meaning the last page).
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
	require.Equal(t, "", next, "a null next_offset should convert to an empty string, meaning the last page")
	require.Equal(t, []CaptionItem{{AssetID: "a2", Text: "一片海", MtimeMs: 200}}, items)
}

// TestListCaptionsNon2xxIsError: a non-2xx (e.g. 503 qdrant unavailable)
// should return an error, and the caller (Puller) silently skips this round based on it.
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

// TestListCaptionsDiscoveryFileMissing: Parser not deployed (discoveryFile
// doesn't exist) should return ErrParserUnavailable, letting Puller silently
// skip the whole round.
func TestListCaptionsDiscoveryFileMissing(t *testing.T) {
	c := New(t.TempDir())
	_, _, err := c.ListCaptions(context.Background(), "")
	require.ErrorIs(t, err, ErrParserUnavailable)
}
