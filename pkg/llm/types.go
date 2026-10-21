// Package llm 是统一的多供应商 LLM 抽象层（对应 pi 的 pi-ai）。
// 它只定义消息模型、流式事件与 Provider 接口，不认识上层 agent。
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Role 是 LLM 认识的消息角色。LLM 只理解这三种角色。
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "toolResult"
)

// StopReason 对应 pi 的 stopReason。
type StopReason string

const (
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"  // 输出被 token 上限截断
	StopToolUse StopReason = "toolUse" // 模型请求工具调用
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// Content 是消息内容块的接口。实现者：TextContent、ThinkingContent、
// ToolCallContent、ImageContent。
type Content interface {
	contentType() string
}

// TextContent 是普通文本块。
type TextContent struct {
	Text string `json:"text"`
}

func (c *TextContent) contentType() string { return "text" }

// ThinkingContent 是思考/推理块。
type ThinkingContent struct {
	Thinking string `json:"thinking"`
}

func (c *ThinkingContent) contentType() string { return "thinking" }

// ToolCallContent 是模型发起的工具调用。
type ToolCallContent struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c *ToolCallContent) contentType() string { return "toolCall" }

// ImageContent 是 base64 图片块。
type ImageContent struct {
	Data     string `json:"data"`     // base64
	MimeType string `json:"mimeType"` // 如 image/png
}

func (c *ImageContent) contentType() string { return "image" }

// Usage 是 token 与成本统计（对应 pi 的 Usage）。
// Input 统一表示包含 cacheRead/cacheWrite 的总输入 token，缓存是 Input 的子项。
type Usage struct {
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheRead   int     `json:"cacheRead,omitempty"`
	CacheWrite  int     `json:"cacheWrite,omitempty"`
	TotalTokens int     `json:"totalTokens"`
	Cost        float64 `json:"cost,omitempty"`
}

// Total 返回本次响应的总 token 数，provider 未报告 totalTokens 时回退到 input+output。
func (u Usage) Total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output
}

// CacheHitRate 返回 cacheRead 占总输入的比例；input 为 0 时没有有效命中率。
func (u Usage) CacheHitRate() (float64, bool) {
	if u.Input <= 0 {
		return 0, false
	}
	return float64(u.CacheRead) / float64(u.Input), true
}

// IsZero 报告 provider 是否未返回任何用量数据。
func (u Usage) IsZero() bool {
	return u == (Usage{})
}

// Add 把另一次响应的用量累加到当前统计。
func (u *Usage) Add(other Usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.CacheWrite += other.CacheWrite
	u.TotalTokens += other.Total()
	u.Cost += other.Cost
}

// Message 是窄的 LLM 消息。assistant 消息才填充 Usage/StopReason 等元数据。
type Message struct {
	Role       Role       `json:"role"`
	Content    []Content  `json:"content"`
	StopReason StopReason `json:"stopReason,omitempty"`
	Usage      Usage      `json:"usage,omitempty"`
	// toolResult 消息字段
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	Timestamp  int64  `json:"timestamp"`
}

// Text 返回消息中全部文本块的拼接，便于打印。
func (m *Message) Text() string {
	var s string
	for _, c := range m.Content {
		if t, ok := c.(*TextContent); ok {
			s += t.Text
		}
	}
	return s
}

// ToolCalls 返回消息中的全部工具调用块。
func (m *Message) ToolCalls() []*ToolCallContent {
	var out []*ToolCallContent
	for _, c := range m.Content {
		if tc, ok := c.(*ToolCallContent); ok {
			out = append(out, tc)
		}
	}
	return out
}

// NewUserMessage 构造一条纯文本 user 消息。
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Content: []Content{&TextContent{Text: text}}, Timestamp: nowMs()}
}

// NewToolResultMessage 构造一条 toolResult 消息。
func NewToolResultMessage(callID, toolName string, content []Content, isErr bool) Message {
	return Message{
		Role:       RoleToolResult,
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
		IsError:    isErr,
		Timestamp:  nowMs(),
	}
}

// ErrorTextResult 生成一条错误文本的 toolResult 消息（对应 pi 的 createErrorToolResult）。
func ErrorTextResult(callID, toolName, msg string) Message {
	return NewToolResultMessage(callID, toolName, []Content{&TextContent{Text: msg}}, true)
}

// Tool 是发给 LLM 的工具描述。Parameters 是 JSON Schema。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Context 是一次 LLM 调用的完整输入（对应 pi 的 Context）。
type Context struct {
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Messages     []Message `json:"messages"`
	Tools        []Tool    `json:"tools,omitempty"`
}

// Model 描述一个模型（对应 pi 的 Model）。
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	BaseURL       string `json:"baseUrl,omitempty"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
	Reasoning     bool   `json:"reasoning"`
}

// StreamOptions 是流式调用的选项。
type StreamOptions struct {
	APIKey      string
	MaxTokens   int
	Temperature *float64
	// Headers 允许注入额外请求头（对应 pi 的 before_provider_headers 场景）。
	Headers map[string]string
}

// RetryPolicy 控制收到成功响应头之前的重试；SSE 消费期间绝不重放请求。
// MaxRetries 不含首次请求，0 表示关闭重试；负数按 0 处理。
// InitialDelay/MaxDelay <= 0 分别使用 250ms/4s。
type RetryPolicy struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// RequestLimiter 可由多个 Provider 共享，每次 HTTP 尝试（包括重试）前调用。
// 实现必须支持并发调用，并在 ctx 取消时停止等待。
type RequestLimiter interface {
	Wait(ctx context.Context) error
}

// ProviderOption 在构造时配置 Provider；运行期间不再修改配置。
type ProviderOption func(*providerHTTPConfig)

// WithBaseURL 覆盖 Provider 的 API 端点（如 Anthropic 兼容网关）；
// 空值保留该 Provider 的默认端点。
func WithBaseURL(baseURL string) ProviderOption {
	return func(cfg *providerHTTPConfig) {
		if baseURL != "" {
			cfg.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithHTTPClient 注入 HTTP client，用于代理、超时或自定义 Transport。
// nil 保留默认 client；Provider 不会修改 client，调用方也不应在使用中修改它。
// Client.Timeout 包含 SSE 读取时间；只限制建连/响应头时应配置 Transport。
func WithHTTPClient(client *http.Client) ProviderOption {
	return func(cfg *providerHTTPConfig) {
		if client != nil {
			cfg.client = client
		}
	}
}

// WithRetryPolicy 替换默认的重试策略（默认最多重试 2 次）。
func WithRetryPolicy(policy RetryPolicy) ProviderOption {
	return func(cfg *providerHTTPConfig) { cfg.retry = policy }
}

// WithRequestLimiter 注入可跨 Provider 共享的限流器；默认不限制速率。
func WithRequestLimiter(limiter RequestLimiter) ProviderOption {
	return func(cfg *providerHTTPConfig) { cfg.limiter = limiter }
}

// StreamFn 是 provider 无关的流式调用函数签名（对应 pi 的 StreamFn）。
// 上层 agent 只依赖它，不依赖任何具体 Provider。
type StreamFn func(ctx context.Context, model Model, llmCtx Context, opts StreamOptions) *EventStream

// ValidateToolArguments 对照 JSON Schema 做基础校验（required 与顶层 type）。
// 对应 pi 的 validateToolArguments 的精简版。
func ValidateToolArguments(tool Tool, args json.RawMessage) error {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return fmt.Errorf("invalid JSON arguments: %w", err)
		}
	} else {
		m = map[string]any{}
	}
	req, _ := tool.Parameters["required"].([]any)
	for _, r := range req {
		name, _ := r.(string)
		if _, ok := m[name]; !ok {
			return fmt.Errorf("missing required argument %q", name)
		}
	}
	props, _ := tool.Parameters["properties"].(map[string]any)
	for k, v := range m {
		prop, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		want, _ := prop["type"].(string)
		if want == "" {
			continue
		}
		if err := checkJSONType(k, v, want); err != nil {
			return err
		}
	}
	return nil
}

func checkJSONType(name string, v any, want string) error {
	ok := false
	switch want {
	case "string":
		_, ok = v.(string)
	case "number", "integer":
		_, ok = v.(float64)
	case "boolean":
		_, ok = v.(bool)
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	default:
		ok = true
	}
	if !ok {
		return fmt.Errorf("argument %q: expected %s", name, want)
	}
	return nil
}
