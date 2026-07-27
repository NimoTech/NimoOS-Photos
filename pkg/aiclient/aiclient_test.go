// Package aiclient 的测试:用 httptest 假 NimoOS-AI _internal 服务验证路径/
// header/模型选择策略/超时行为,不接触真实 AI 服务。
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

// writeAIURL 把假 AI 服务地址写进发现文件,返回该文件路径。
func writeAIURL(t *testing.T, url string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ai.url")
	require.NoError(t, os.WriteFile(p, []byte(url), 0o644))
	return p
}

// stubEmptyUsersDB 把包级 usersDBPath 指向一个必然不存在的路径,使
// resolveUserID 必然兜底 "system"——这台开发机本身跑着真实 NimoOS
// user-service,默认路径 /var/lib/nimoos/db/user.db 可能真实存在,测试必须
// 显式指向空路径才是确定性的("无 user.db"场景),而不是依赖跑测试的机器
// 上没有装 NimoOS。
func stubEmptyUsersDB(t *testing.T) {
	t.Helper()
	orig := usersDBPath
	t.Cleanup(func() { usersDBPath = orig })
	usersDBPath = filepath.Join(t.TempDir(), "no-such-user.db")
}

// TestCompletePicksLocalModelAndPostsChat:local 非空时优先本地模型,chat
// 请求路径/header 正确,且不带 X-NimoOS-Force-Cloud。
func TestCompletePicksLocalModelAndPostsChat(t *testing.T) {
	stubEmptyUsersDB(t)
	var gotModelsPath, gotChatPath string
	var gotUserIDHeader, gotForceCloudHeader string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/_internal/models":
			gotModelsPath = r.URL.Path
			require.Equal(t, "system", r.URL.Query().Get("user_id"), "无 user.db 时应兜底 system")
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
	require.Equal(t, "system", gotUserIDHeader, "无 user.db 时兜底常量 system")
	require.Empty(t, gotForceCloudHeader, "本地有模型时不应强制云端")
	require.Equal(t, "qwen2.5:7b", gotBody["model"])
}

// TestCompleteChatBodyCapsMaxTokensAndTemperature:请求体必须带
// max_tokens/temperature 约束(对照 wiki_summary_worker/llm.py 同款防御)——
// NimoOS-AI 云端适配层 max_tokens 缺省高达 16000,起名这种短输出调用不加
// 约束会放大云端调用的成本/延迟。
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

// TestCompleteFallsBackToCloudModel:local 为空、cloud 非空时选云端默认模型,
// 且带上 X-NimoOS-Force-Cloud: true。
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

// TestCompleteNoModelAvailableIsError:local/cloud 都为空时返回 error,不发起
// chat 请求。
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
	require.False(t, chatCalled, "没有可用模型时不应发 chat 请求")
}

// TestCompleteChatNon2xxIsError: chat completions 返回非 2xx → error。
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

// TestCompleteAIURLFileMissingIsError:发现文件不存在(AI 未部署)→ error,
// 不 panic。
func TestCompleteAIURLFileMissingIsError(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "no-such-ai.url"))
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAIUnavailable)
}

// TestCompleteTimesOutWithin5Seconds:models 端点挂起不回应时,Complete 应在
// completeTimeout(5s)左右超时返回,而不是无限等待。
func TestCompleteTimesOutWithin5Seconds(t *testing.T) {
	stubEmptyUsersDB(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 永远不回应,逼 Complete 走 ctx 超时路径
	}))
	// 关闭顺序很关键:必须先放行还挂着的 handler(close(block))再
	// srv.Close(),否则 Close() 会等待那条连接收尾,和 <-block 死锁。
	defer srv.Close()
	defer close(block)

	c := New(writeAIURL(t, srv.URL))
	start := time.Now()
	_, err := c.Complete(context.Background(), "x")
	elapsed := time.Since(start)
	require.Error(t, err)
	require.Less(t, elapsed, 7*time.Second, "5s 超时应生效,不应显著超出")
	require.GreaterOrEqual(t, elapsed, 4*time.Second, "不应远早于 5s 超时就返回")
}

// TestResolveUserIDPrefersAdminThenAnyThenSystem:user.db 存在且有 admin 用户
// 时,header 携带该 admin 的最小 id;无 admin 但有其它用户时取最小 id;
// user.db 不存在时兜底 "system"(顶部测试已覆盖这一支)。
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
	require.Equal(t, "2", gotUserID, "应取 role=admin 中最小的 id")
}
