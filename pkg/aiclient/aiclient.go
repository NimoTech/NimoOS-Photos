// Package aiclient 是 NimoOS-AI `_internal` 接口的薄客户端,供 Smart Moments
// 的 LLM 命名(best-effort)使用。`_internal` 分组 localhost-only、免 JWT
// (见 NimoOS-AI route/v2.go:93 `g.Group("/_internal", LocalhostOnly)`),不
// 经 Gateway/JWT 鉴权,是 wiki_summary_worker 同款的直连方式。
package aiclient

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // CGO SQLite3 驱动,resolveUserID 只读打开 user.db 用
)

// defaultAIURLFile 是 AI 服务发现文件的默认路径:NimoOS-AI 启动时把自己的
// 地址写到这里(见 wiki_summary_worker/discovery.py:ai_url 同一份文件)。
const defaultAIURLFile = "/var/run/nimoos/ai.url"

// usersDBPath 是 user.db 的路径,resolveUserID 用它兜底取 admin 用户 id;
// 与 wiki_summary_worker/discovery.py 的 _USERS_DB 常量同一份文件。
// 包级变量而非常量,是为了让白盒测试(同包)能重写指向临时库。
var usersDBPath = "/var/lib/nimoos/db/user.db"

// completeTimeout 是 Complete 一次调用(选模型 + chat completions 两次请求
// 合计)的整体超时。
const completeTimeout = 5 * time.Second

// ErrAIUnavailable 表示发现文件缺失/不可读(AI 服务未部署或未启动),调用方
// 应据此静默跳过(best-effort)。
var ErrAIUnavailable = errors.New("ai service not available")

// Client 是 NimoOS-AI `_internal` 接口的薄客户端。
type Client struct {
	aiURLFile string
	httpc     *http.Client
}

// New 构造 Client。aiURLFile 为空时用默认路径 /var/run/nimoos/ai.url;每次
// 调用都重新读这个文件(AI 服务重启换随机端口时自适应),照
// pkg/parserclient 的发现惯例。
func New(aiURLFile string) *Client {
	if aiURLFile == "" {
		aiURLFile = defaultAIURLFile
	}
	return &Client{
		aiURLFile: aiURLFile,
		httpc:     &http.Client{},
	}
}

func (c *Client) baseURL() (string, error) {
	b, err := os.ReadFile(c.aiURLFile)
	if err != nil {
		return "", ErrAIUnavailable
	}
	u := strings.TrimSpace(string(b))
	if u == "" {
		return "", ErrAIUnavailable
	}
	return u, nil
}

// internalModelsResponse 对应 GET /v1/ai/_internal/models 的响应体(只取
// Complete 选模型用得到的字段,其余字段原样忽略)。
type internalModelsResponse struct {
	Local []struct {
		Name string `json:"name"`
	} `json:"local"`
	Cloud []struct {
		DefaultModel string `json:"default_model"`
	} `json:"cloud"`
}

// pickModel 选模型 + 是否需要强制走云端。策略移植自
// NimoOS-AI/wiki_summary_worker/discovery.py:41-79 的
// resolve_model_and_routing:优先本地(Ollama)模型,本地为空才退回该用户
// 已启用的云端 provider 默认模型;两者皆空返回 error(调用方 best-effort
// 静默跳过)。
func pickModel(ctx context.Context, httpc *http.Client, base, userID string) (model string, forceCloud bool, err error) {
	reqURL := base + "/v1/ai/_internal/models?user_id=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", false, fmt.Errorf("aiclient: models HTTP %d", resp.StatusCode)
	}
	var parsed internalModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", false, fmt.Errorf("aiclient: decode models: %w", err)
	}
	if len(parsed.Local) > 0 {
		return parsed.Local[0].Name, false, nil
	}
	if len(parsed.Cloud) > 0 {
		return parsed.Cloud[0].DefaultModel, true, nil
	}
	return "", false, errors.New("aiclient: no model available (no local Ollama model, no enabled cloud provider)")
}

// chatMessage 是 OpenAI messages 格式的最小子集。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionsResponse 是 chat completions 响应体里 Complete 用得到的部分。
type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete 向 NimoOS-AI 发一次单轮 chat completion,返回模型输出的纯文本。
// 整体(选模型 + chat completions 两次请求)5s 超时;发现文件缺失、HTTP
// 非 2xx、无可用模型、超时均返回 error——调用方(Smart Moments 的 LLM
// 命名)据此静默跳过,绝不能阻塞主流程。
func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()

	base, err := c.baseURL()
	if err != nil {
		return "", err
	}

	userID := resolveUserID()

	model, forceCloud, err := pickModel(ctx, c.httpc, base, userID)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []chatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v1/ai/_internal/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NimoOS-User-ID", userID)
	if forceCloud {
		req.Header.Set("X-NimoOS-Force-Cloud", "true")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("aiclient: chat completions HTTP %d", resp.StatusCode)
	}
	var parsed chatCompletionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("aiclient: decode chat completions: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("aiclient: empty choices in chat completions response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

// resolveUserID 取 X-NimoOS-User-ID 请求头值,策略完全对齐
// NimoOS-AI/wiki_summary_worker/discovery.py 的 resolve_user_id 第 2/3/4 步
// (本客户端没有它第 1 步那种运维配置覆盖项):
//  1. user.db 里 role='admin' 的最小 id;
//  2. 查不到 admin 则任意用户的最小 id;
//  3. user.db 不可读/查询失败/空表,一律兜底常量 "system"——机器上没有
//     user.db 时 chat-completions 会路由到本地 Ollama,是合理的兜底,worker
//     不会因此崩溃。
func resolveUserID() string {
	db, err := sql.Open("sqlite3", "file:"+usersDBPath+"?mode=ro&_busy_timeout=2000")
	if err != nil {
		return "system"
	}
	defer db.Close()

	var id int64
	if err := db.QueryRow(`SELECT id FROM o_users WHERE role='admin' ORDER BY id LIMIT 1`).Scan(&id); err == nil {
		return strconv.FormatInt(id, 10)
	}
	if err := db.QueryRow(`SELECT id FROM o_users ORDER BY id LIMIT 1`).Scan(&id); err == nil {
		return strconv.FormatInt(id, 10)
	}
	return "system"
}
