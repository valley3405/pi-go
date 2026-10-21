package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"goagent/pkg/llm"
	"goagent/pkg/tools"
)

func setupWebEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("GOAGENT_WEB_BASE_URL", baseURL)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
}

func TestWebSearchFormatsResults(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"DeepSeek V4 模型发布","url":"https://example.com/v4","snippet":"短摘要","summary":"长摘要","site_name":"Example News","date":"2026-06-01T00:00:00+00:00"},
			{"title":"另一条","url":"https://example.com/other","snippet":"s2","summary":"s2","site_name":null,"date":null}
		]}`)
	}))
	defer server.Close()
	setupWebEnv(t, server.URL)

	res, err := tools.WebSearch().Execute(context.Background(), "c1", jargs(map[string]any{"query": "DeepSeek V4", "count": 5}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization header = %q", gotAuth)
	}
	if gotPath != "/v1/tools/search" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["query"] != "DeepSeek V4" || gotBody["count"] != float64(5) {
		t.Errorf("request body = %v", gotBody)
	}
	if _, hasContent := gotBody["need_content"]; hasContent {
		t.Errorf("need_content should be omitted when false: %v", gotBody)
	}
	out := res.Content[0].(*llm.TextContent).Text
	for _, want := range []string{"## 1. DeepSeek V4 模型发布", "https://example.com/v4", "Example News, 2026-06-01", "长摘要", "## 2. 另一条"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWebSearchEmptyAndNoKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()
	setupWebEnv(t, server.URL)

	search := tools.WebSearch()
	res, err := search.Execute(context.Background(), "c1", jargs(map[string]any{"query": "  "}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "query is required") {
		t.Errorf("blank query output: %s", out)
	}

	res, err = search.Execute(context.Background(), "c2", jargs(map[string]any{"query": "nothing matches"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; out != "no results" {
		t.Errorf("empty results output: %s", out)
	}

	t.Setenv("DEEPSEEK_API_KEY", "")
	res, err = search.Execute(context.Background(), "c3", jargs(map[string]any{"query": "x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "missing API key") {
		t.Errorf("missing key output: %s", out)
	}
}

func TestWebSearchErrorAndRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"title":"ok","url":"https://example.com"}]}`)
	}))
	defer server.Close()
	setupWebEnv(t, server.URL)

	res, err := tools.WebSearch().Execute(context.Background(), "c1", jargs(map[string]any{"query": "retry me"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "ok") {
		t.Errorf("retry should succeed: %s", out)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}

	server.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer bad.Close()
	setupWebEnv(t, bad.URL)
	res, err = tools.WebSearch().Execute(context.Background(), "c2", jargs(map[string]any{"query": "x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "HTTP 401: invalid key") {
		t.Errorf("401 output: %s", out)
	}
}

func TestWebFetch(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path != "/v1/tools/fetch" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"title":"Example article","content":"# Example article\n\nBody."}`)
	}))
	defer server.Close()
	setupWebEnv(t, server.URL)

	fetch := tools.WebFetch()
	res, err := fetch.Execute(context.Background(), "c1", jargs(map[string]any{"url": "https://example.com/article"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["url"] != "https://example.com/article" {
		t.Errorf("request body = %v", gotBody)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "# Example article") || !strings.Contains(out, "Body.") {
		t.Errorf("fetch output: %s", out)
	}

	for _, badURL := range []string{"ftp://example.com", "example.com", "https://user:pass@example.com/"} {
		res, err = fetch.Execute(context.Background(), "c2", jargs(map[string]any{"url": badURL}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "error:") {
			t.Errorf("url %q should be rejected: %s", badURL, out)
		}
	}
}
