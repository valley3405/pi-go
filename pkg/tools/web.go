package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// DeepSeek Search/Fetch API（https://api-docs.deepseek.com/zh-cn/guides/search）。
// 两者均为 POST + Bearer 鉴权，错误为 OpenAI 兼容格式。

const (
	defaultWebBaseURL  = "https://api.deepseek.com"
	webMaxResponseSize = 4 << 20
	webMaxRetries      = 2
)

var webHTTPClient = &http.Client{Timeout: 60 * time.Second}

// WebSearch 通过 DeepSeek Search 接口搜索网页，返回标题、链接与摘要。
func WebSearch() agent.AgentTool {
	return &webSearchTool{baseTool{
		name: "web_search",
		tool: llm.Tool{
			Name: "web_search",
			Description: "Search the web and return titles, URLs and summaries. " +
				"The query supports operators: site:example.com, -site:example.com, " +
				"lang:zh,en, after:2025 or after:2025-01-01, before:2026, \"exact phrase\", -excluded.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        stringProp("Search query, 1-128 characters"),
					"count":        intProp("Number of results, 1-50 (default 10)"),
					"need_content": boolProp("Also return full page content per result; output grows significantly (default false)"),
				},
				"required": []any{"query"},
			},
		},
	}}
}

type webSearchTool struct{ baseTool }

func (t *webSearchTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Query       string `json:"query"`
		Count       int    `json:"count"`
		NeedContent bool   `json:"need_content"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(p.Query) == "" {
		return textResult("error: query is required"), nil
	}
	if p.Count <= 0 {
		p.Count = 10
	}
	if p.Count > 50 {
		p.Count = 50
	}
	payload := map[string]any{"query": p.Query, "count": p.Count}
	if p.NeedContent {
		payload["need_content"] = true
	}
	var resp struct {
		Results []struct {
			Title    string  `json:"title"`
			URL      string  `json:"url"`
			Snippet  string  `json:"snippet"`
			Summary  string  `json:"summary"`
			Content  string  `json:"content"`
			SiteName *string `json:"site_name"`
			Date     *string `json:"date"`
		} `json:"results"`
	}
	if err := postWebTool(ctx, "search", payload, &resp); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	if len(resp.Results) == 0 {
		return textResult("no results"), nil
	}
	var b strings.Builder
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "## %d. %s\n%s\n", i+1, r.Title, r.URL)
		var meta []string
		if r.SiteName != nil && *r.SiteName != "" {
			meta = append(meta, *r.SiteName)
		}
		if r.Date != nil && *r.Date != "" {
			meta = append(meta, *r.Date)
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "(%s)\n", strings.Join(meta, ", "))
		}
		if r.Snippet != "" {
			fmt.Fprintf(&b, "%s\n", r.Snippet)
		}
		if r.Summary != "" && r.Summary != r.Snippet {
			fmt.Fprintf(&b, "%s\n", r.Summary)
		}
		if r.Content != "" {
			fmt.Fprintf(&b, "\n%s\n", r.Content)
		}
		if i < len(resp.Results)-1 {
			b.WriteString("\n")
		}
	}
	return textResult(b.String()), nil
}

// WebFetch 通过 DeepSeek Fetch 接口抓取单个网页，返回 Markdown 正文。
func WebFetch() agent.AgentTool {
	return &webFetchTool{baseTool{
		name: "web_fetch",
		tool: llm.Tool{
			Name:        "web_fetch",
			Description: "Fetch a single web page and return its content as Markdown. The URL must be http/https without credentials.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": stringProp("HTTP/HTTPS URL of the page to fetch"),
				},
				"required": []any{"url"},
			},
		},
	}}
}

type webFetchTool struct{ baseTool }

func (t *webFetchTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return textResult("error: url must be an HTTP/HTTPS URL with a host"), nil
	}
	if u.User != nil {
		return textResult("error: url must not contain a username or password"), nil
	}
	var resp struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := postWebTool(ctx, "fetch", map[string]any{"url": p.URL}, &resp); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	if resp.Title == "" {
		return textResult(resp.Content), nil
	}
	return textResult("# " + resp.Title + "\n\n" + resp.Content), nil
}

// postWebTool 调用 Search/Fetch 接口；408/429/5xx 按文档建议等待后重试。
func postWebTool(ctx context.Context, api string, payload, out any) error {
	key := webAPIKey()
	if key == "" {
		return fmt.Errorf("missing API key: set DEEPSEEK_API_KEY or OPENAI_API_KEY")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := webEndpoint(api)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := webHTTPClient.Do(req)
		if err != nil {
			if attempt < webMaxRetries && ctx.Err() == nil {
				if waitErr := webBackoff(ctx, attempt, ""); waitErr != nil {
					return waitErr
				}
				continue
			}
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, webMaxResponseSize))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusOK {
			return json.Unmarshal(data, out)
		}
		if webRetryable(resp.StatusCode) && attempt < webMaxRetries {
			if waitErr := webBackoff(ctx, attempt, resp.Header.Get("Retry-After")); waitErr != nil {
				return waitErr
			}
			continue
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, webErrorMessage(data))
	}
}

func webRetryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func webBackoff(ctx context.Context, attempt int, retryAfter string) error {
	wait := time.Duration(attempt+1) * time.Second
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		wait = time.Duration(seconds) * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func webErrorMessage(data []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
		return trimmed
	}
	return "request failed"
}

func webAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
}

// webEndpoint 允许 base URL 带或不带 /v1 后缀（与 OPENAI_BASE_URL 的习惯写法兼容）。
func webEndpoint(api string) string {
	base := webBaseURL()
	if strings.HasSuffix(base, "/v1") {
		return base + "/tools/" + api
	}
	return base + "/v1/tools/" + api
}

func webBaseURL() string {
	if base := strings.TrimSpace(os.Getenv("GOAGENT_WEB_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	if base := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); base != "" {
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return defaultWebBaseURL
}
