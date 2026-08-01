// Package aiclient is a thin client for NimoOS-AI's `_internal` interface,
// used by Smart Moments' (best-effort) LLM naming. The `_internal` group is
// localhost-only and JWT-exempt (see NimoOS-AI route/v2.go:93
// `g.Group("/_internal", LocalhostOnly)`), bypassing Gateway/JWT auth — the
// same direct-connection approach used by wiki_summary_worker.
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

	_ "github.com/mattn/go-sqlite3" // CGO SQLite3 driver, used by resolveUserID to open user.db read-only
)

// defaultAIURLFile is the default path for the AI service discovery file:
// NimoOS-AI writes its own address here on startup (see the same file
// referenced by wiki_summary_worker/discovery.py:ai_url).
const defaultAIURLFile = "/var/run/nimoos/ai.url"

// usersDBPath is the path to user.db, used by resolveUserID as a fallback to
// get the admin user id; the same file referenced by
// wiki_summary_worker/discovery.py's _USERS_DB constant.
// A package-level var rather than a const so white-box tests (same package)
// can override it to point at a temp DB.
var usersDBPath = "/var/lib/nimoos/db/user.db"

// completeTimeout is the overall timeout for one Complete call (model
// selection + chat completions requests combined). Real-machine acceptance
// testing found weaker local models occasionally slower than 5s (naming
// requests would time out and fall back best-effort, leaving the template
// title); relaxed to 10s to trade for a higher LLM naming success rate.
const completeTimeout = 10 * time.Second

// chatMaxTokens/chatTemperature are defensive constraints on the chat
// completions request body (mirroring the same approach in
// wiki_summary_worker/llm.py): Complete is only used to generate a short
// title of "at most 4 words", and NimoOS-AI's cloud adapter layer defaults
// max_tokens as high as 16000 — without this cap it would needlessly inflate
// the cost/latency of the cloud call; lowering the temperature makes the
// output style more consistent.
const (
	chatMaxTokens   = 60
	chatTemperature = 0.2
)

// ErrAIUnavailable indicates the discovery file is missing/unreadable (the
// AI service isn't deployed or isn't running); the caller should silently
// skip (best-effort) based on this.
var ErrAIUnavailable = errors.New("ai service not available")

// Client is a thin client for NimoOS-AI's `_internal` interface.
type Client struct {
	aiURLFile string
	httpc     *http.Client
}

// New constructs a Client. If aiURLFile is empty, uses the default path
// /var/run/nimoos/ai.url; the file is re-read on every call (adapting when
// the AI service restarts on a new random port), following pkg/parserclient's
// discovery convention.
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

// internalModelsResponse corresponds to the response body of GET
// /v1/ai/_internal/models (only the fields Complete's model selection needs;
// other fields are ignored as-is).
type internalModelsResponse struct {
	Local []struct {
		Name string `json:"name"`
	} `json:"local"`
	Cloud []struct {
		DefaultModel string `json:"default_model"`
	} `json:"cloud"`
}

// pickModel selects a model and whether the cloud must be forced. The
// strategy is ported from resolve_model_and_routing in
// NimoOS-AI/wiki_summary_worker/discovery.py:41-79: prefer a local (Ollama)
// model, falling back to that user's enabled cloud provider's default model
// only if there's no local model; if both are empty, returns an error (the
// caller silently skips, best-effort).
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

// chatMessage is a minimal subset of the OpenAI messages format.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionsResponse is the part of the chat completions response body that Complete needs.
type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete sends a single-turn chat completion to NimoOS-AI, returning the
// model's plain-text output.
// Overall (model selection + chat completions requests combined) 5s timeout;
// a missing discovery file, non-2xx HTTP, no available model, or a timeout
// all return an error — the caller (Smart Moments' LLM naming) silently
// skips based on this, and must never block the main flow.
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

	// max_tokens/temperature mirror the same defensive pattern as
	// wiki_summary_worker/llm.py: NimoOS-AI's cloud adapter layer defaults
	// max_tokens as high as 16000, and a short-output call like naming would
	// inflate cloud call cost/latency without this cap; a lower temperature
	// makes the title style more consistent, with fewer wild associations.
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []chatMessage{{Role: "user", Content: prompt}},
		"max_tokens":  chatMaxTokens,
		"temperature": chatTemperature,
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

// resolveUserID gets the X-NimoOS-User-ID request header value, with a
// strategy fully aligned with steps 2/3/4 of resolve_user_id in
// NimoOS-AI/wiki_summary_worker/discovery.py (this client has no equivalent
// of its step 1, the ops-config override):
//  1. The smallest id among user.db rows with role='admin';
//  2. If no admin is found, the smallest id among any users;
//  3. If user.db is unreadable/the query fails/the table is empty, fall back
//     to the constant "system" — on a machine with no user.db,
//     chat-completions routes to local Ollama, which is a reasonable
//     fallback; the worker won't crash over this.
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
