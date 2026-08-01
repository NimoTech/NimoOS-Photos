// Tests for package aiclient: uses an httptest fake NimoOS-AI _internal
// service to verify path/header/model-selection strategy/timeout behavior,
// without touching a real AI service.
package aiclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// writeAIURL writes the fake AI service address into the discovery file, returning that file's path.
func writeAIURL(t *testing.T, url string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ai.url")
	require.NoError(t, os.WriteFile(p, []byte(url), 0o644))
	return p
}

// stubEmptyUsersDB points the package-level usersDBPath at a path guaranteed
// not to exist, so resolveUserID is guaranteed to fall back to "system" —
// this dev machine may itself be running a real NimoOS user-service, so the
// default path /var/lib/nimoos/db/user.db might genuinely exist; the test
// must explicitly point at an empty path to be deterministic (the "no
// user.db" scenario), rather than depending on the test machine not having
// NimoOS installed.
func stubEmptyUsersDB(t *testing.T) {
	t.Helper()
	orig := usersDBPath
	t.Cleanup(func() { usersDBPath = orig })
	usersDBPath = filepath.Join(t.TempDir(), "no-such-user.db")
}

// TestCompletePicksLocalModelAndPostsChat: when local is non-empty, prefers
// the local model; the chat request path/headers are correct, and
// X-NimoOS-Force-Cloud is not set.
func TestCompletePicksLocalModelAndPostsChat(t *testing.T) {
	stubEmptyUsersDB(t)
	var gotModelsPath, gotChatPath string
	var gotUserIDHeader, gotForceCloudHeader string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			gotModelsPath = r.URL.Path
			require.Equal(t, "system", r.URL.Query().Get("user_id"), "should fall back to system when there's no user.db")
			w.Write([]byte(`{"local":[{"name":"qwen2.5:7b"}],"cloud":[{"default_model":"gpt-4o"}]}`))
		case "/v1/ai/_internal/chat/completions":
			gotChatPath = r.URL.Path
			gotUserIDHeader = r.Header.Get("X-NimoOS-User-ID")
			gotForceCloudHeader = r.Header.Get("X-NimoOS-Force-Cloud")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.Write([]byte(`{"choices":[{"message":{"content":"Sunset Beach"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	title, err := c.Complete(context.Background(), "give me a title")
	require.NoError(t, err)
	require.Equal(t, "Sunset Beach", title)

	require.Equal(t, "/v1/ai/_internal/models", gotModelsPath)
	require.Equal(t, "/v1/ai/_internal/chat/completions", gotChatPath)
	require.Equal(t, "system", gotUserIDHeader, "falls back to the constant system when there's no user.db")
	require.Empty(t, gotForceCloudHeader, "should not force cloud when a local model is available")
	require.Equal(t, "qwen2.5:7b", gotBody["model"])
}

// TestCompleteChatBodyCapsMaxTokensAndTemperature: the request body must
// carry the max_tokens/temperature constraints (mirroring the same defensive
// pattern in wiki_summary_worker/llm.py) — NimoOS-AI's cloud adapter layer
// defaults max_tokens as high as 16000, and a short-output call like naming
// would inflate cloud call cost/latency without this cap.
func TestCompleteChatBodyCapsMaxTokensAndTemperature(t *testing.T) {
	stubEmptyUsersDB(t)
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			w.Write([]byte(`{"local":[{"name":"m1"}],"cloud":[]}`))
		case "/v1/ai/_internal/chat/completions":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	_, err := c.Complete(context.Background(), "x")
	require.NoError(t, err)
	require.Equal(t, float64(60), gotBody["max_tokens"])
	require.Equal(t, 0.2, gotBody["temperature"])
}

// TestCompleteFallsBackToCloudModel: when local is empty and cloud is
// non-empty, picks the cloud default model and sets X-NimoOS-Force-Cloud: true.
func TestCompleteFallsBackToCloudModel(t *testing.T) {
	stubEmptyUsersDB(t)
	var gotForceCloudHeader string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			w.Write([]byte(`{"local":[],"cloud":[{"default_model":"gpt-4o-mini"}]}`))
		case "/v1/ai/_internal/chat/completions":
			gotForceCloudHeader = r.Header.Get("X-NimoOS-Force-Cloud")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.Write([]byte(`{"choices":[{"message":{"content":"Family Trip"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	title, err := c.Complete(context.Background(), "title please")
	require.NoError(t, err)
	require.Equal(t, "Family Trip", title)
	require.Equal(t, "true", gotForceCloudHeader)
	require.Equal(t, "gpt-4o-mini", gotBody["model"])
}

// TestCompleteNoModelAvailableIsError: when both local/cloud are empty,
// returns an error and doesn't issue a chat request.
func TestCompleteNoModelAvailableIsError(t *testing.T) {
	stubEmptyUsersDB(t)
	chatCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ai/_internal/chat/completions" {
			chatCalled = true
		}
		w.Write([]byte(`{"local":[],"cloud":[]}`))
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
	require.False(t, chatCalled, "should not issue a chat request when no model is available")
}

// TestCompleteChatNon2xxIsError: chat completions returning non-2xx -> error.
func TestCompleteChatNon2xxIsError(t *testing.T) {
	stubEmptyUsersDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			w.Write([]byte(`{"local":[{"name":"m1"}],"cloud":[]}`))
		case "/v1/ai/_internal/chat/completions":
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
}

// TestCompleteAIURLFileMissingIsError: discovery file doesn't exist (AI not
// deployed) -> error, no panic.
func TestCompleteAIURLFileMissingIsError(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "no-such-ai.url"))
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAIUnavailable)
}

// TestCompleteTimesOutWithin10Seconds: when the models endpoint hangs without
// responding, Complete should time out and return around completeTimeout
// (10s), not wait forever.
func TestCompleteTimesOutWithin10Seconds(t *testing.T) {
	stubEmptyUsersDB(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds, forcing Complete down the ctx timeout path
	}))
	// Shutdown order matters: the still-hanging handler must be released
	// (close(block)) before srv.Close(), otherwise Close() waits for that
	// connection to finish and deadlocks against <-block.
	defer srv.Close()
	defer close(block)

	c := New(writeAIURL(t, srv.URL))
	start := time.Now()
	_, err := c.Complete(context.Background(), "x")
	elapsed := time.Since(start)
	require.Error(t, err)
	require.Less(t, elapsed, 12*time.Second, "the 10s timeout should take effect and not be significantly exceeded")
	require.GreaterOrEqual(t, elapsed, 9*time.Second, "should not return well before the 10s timeout")
}

// TestResolveUserIDPrefersAdminThenAnyThenSystem: when user.db exists and has
// an admin user, the header carries that admin's smallest id; with no admin
// but other users, takes the smallest id; when user.db doesn't exist, falls
// back to "system" (already covered by the test above).
func TestResolveUserIDPrefersAdminThenAnyThenSystem(t *testing.T) {
	orig := usersDBPath
	defer func() { usersDBPath = orig }()

	dbPath := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE o_users(id INTEGER PRIMARY KEY, role TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO o_users(id, role) VALUES (5, 'member'), (2, 'admin'), (9, 'admin')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	usersDBPath = dbPath

	var gotUserID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			gotUserID = r.URL.Query().Get("user_id")
			w.Write([]byte(`{"local":[{"name":"m1"}],"cloud":[]}`))
		case "/v1/ai/_internal/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))
	defer srv.Close()

	c := New(writeAIURL(t, srv.URL))
	_, err = c.Complete(context.Background(), "x")
	require.NoError(t, err)
	require.Equal(t, "2", gotUserID, "should take the smallest id among role=admin rows")
}
