package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIProvider 兼容 OpenAI Chat Completions 及其兼容服务
// （DeepSeek、OpenRouter、各类网关），SSE 流式 + tool_calls 增量合并。
type OpenAIProvider struct {
	id      string
	baseURL string
	models  []Model
	client  *http.Client
}

// NewOpenAIProvider 创建 OpenAI 兼容 provider。
// id 用于注册表键（如 "openai"、"deepseek"）；baseURL 如 "https://api.openai.com/v1"。
func NewOpenAIProvider(id, baseURL string, models []Model) *OpenAIProvider {
	for i := range models {
		models[i].Provider = id
		models[i].BaseURL = baseURL
	}
	return &OpenAIProvider{id: id, baseURL: strings.TrimRight(baseURL, "/"), models: models, client: &http.Client{}}
}

func (p *OpenAIProvider) ID() string      { return p.id }
func (p *OpenAIProvider) Models() []Model { return p.models }

// ---- OpenAI wire 格式 ----

type oaiRequest struct {
	Model       string         `json:"model"`
	Messages    []oaiMessage   `json:"messages"`
	Tools       []oaiTool      `json:"tools,omitempty"`
	Stream      bool           `json:"stream"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	StreamOpts  *oaiStreamOpts `json:"stream_options,omitempty"`
}

type oaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content,omitempty"` // string 或 []oaiContentPart
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaiToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content          string        `json:"content"`
			ReasoningContent string        `json:"reasoning_content"` // 推理模型（DeepSeek-R1、QwQ 等）的思考增量
			ToolCalls        []oaiToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func toOAIMessages(c Context) []oaiMessage {
	var out []oaiMessage
	if c.SystemPrompt != "" {
		out = append(out, oaiMessage{Role: "system", Content: c.SystemPrompt})
	}
	for _, m := range c.Messages {
		switch m.Role {
		case RoleUser:
			out = append(out, oaiMessage{Role: "user", Content: toOAIContent(m.Content)})
		case RoleAssistant:
			om := oaiMessage{Role: "assistant"}
			om.Content = m.Text()
			for _, tc := range m.ToolCalls() {
				call := oaiToolCall{ID: tc.ID, Type: "function"}
				call.Function.Name = tc.Name
				call.Function.Arguments = string(tc.Arguments)
				om.ToolCalls = append(om.ToolCalls, call)
			}
			out = append(out, om)
		case RoleToolResult:
			out = append(out, oaiMessage{Role: "tool", Content: m.Text(), ToolCallID: m.ToolCallID})
		}
	}
	return out
}

func toOAIContent(content []Content) any {
	if len(content) == 1 {
		if t, ok := content[0].(*TextContent); ok {
			return t.Text
		}
	}
	var parts []oaiContentPart
	for _, c := range content {
		switch v := c.(type) {
		case *TextContent:
			parts = append(parts, oaiContentPart{Type: "text", Text: v.Text})
		case *ImageContent:
			parts = append(parts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:" + v.MimeType + ";base64," + v.Data}})
		}
	}
	return parts
}

func (p *OpenAIProvider) Stream(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream {
	s := NewEventStream()
	go func() {
		err := p.stream(ctx, model, c, opts, s)
		if err != nil {
			msg := Message{Role: RoleAssistant, StopReason: StopError, Timestamp: nowMs()}
			s.Finish(msg, err)
		}
	}()
	return s
}

func (p *OpenAIProvider) stream(ctx context.Context, model Model, c Context, opts StreamOptions, s *EventStream) error {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.MaxTokens
	}
	reqBody := oaiRequest{
		Model:       model.ID,
		Messages:    toOAIMessages(c),
		Stream:      true,
		MaxTokens:   maxTokens,
		Temperature: opts.Temperature,
		StreamOpts:  &oaiStreamOpts{IncludeUsage: true},
	}
	for _, t := range c.Tools {
		reqBody.Tools = append(reqBody.Tools, oaiTool{
			Type:     "function",
			Function: oaiFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(b))
	}

	return consumeOAISSE(resp.Body, model, s)
}

// consumeOAISSE 解析 SSE 并合并 tool_calls 增量，产出统一事件流。
func consumeOAISSE(r io.Reader, model Model, s *EventStream) error {
	partial := Message{Role: RoleAssistant, Timestamp: nowMs()}
	s.Push(Event{Type: EventStart, Partial: partial})

	textOpen := false
	textBuf := ""
	thinkingOpen := false
	thinkingBuf := ""
	// 按 index 合并流式 tool_calls
	type tcAcc struct {
		id, name, args string
		started        bool
	}
	accByIndex := map[int]*tcAcc{}
	var order []int
	usage := Usage{}
	finishReason := ""

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 容忍注释/心跳行
		}
		if chunk.Usage != nil {
			usage.Input = chunk.Usage.PromptTokens
			usage.Output = chunk.Usage.CompletionTokens
			usage.TotalTokens = chunk.Usage.TotalTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		if d := choice.Delta.ReasoningContent; d != "" {
			if !thinkingOpen {
				s.Push(Event{Type: EventThinkingStart, Partial: partial})
				thinkingOpen = true
			}
			thinkingBuf += d
			p := partial
			p.Content = append(append([]Content{}, partial.Content...), &ThinkingContent{Thinking: thinkingBuf})
			s.Push(Event{Type: EventThinkingDelta, Delta: d, Partial: p})
		}
		if d := choice.Delta.Content; d != "" {
			if thinkingOpen {
				// 思考块结束，转入正式文本
				partial.Content = append(partial.Content, &ThinkingContent{Thinking: thinkingBuf})
				s.Push(Event{Type: EventThinkingEnd, Partial: partial})
				thinkingOpen = false
			}
			if !textOpen {
				s.Push(Event{Type: EventTextStart, Partial: partial})
				textOpen = true
			}
			textBuf += d
			p := partial
			p.Content = append(append([]Content{}, partial.Content...), &TextContent{Text: textBuf})
			s.Push(Event{Type: EventTextDelta, Delta: d, Partial: p})
		}
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := accByIndex[tc.Index]
			if !ok {
				acc = &tcAcc{}
				accByIndex[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name += tc.Function.Name
			}
			if !acc.started {
				s.Push(Event{Type: EventToolCallStart, Partial: partial})
				acc.started = true
			}
			if tc.Function.Arguments != "" {
				acc.args += tc.Function.Arguments
				s.Push(Event{Type: EventToolCallDelta, Delta: tc.Function.Arguments, Partial: partial})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if thinkingOpen {
		partial.Content = append(partial.Content, &ThinkingContent{Thinking: thinkingBuf})
		s.Push(Event{Type: EventThinkingEnd, Partial: partial})
	}
	if textOpen {
		partial.Content = append(partial.Content, &TextContent{Text: textBuf})
		s.Push(Event{Type: EventTextEnd, Partial: partial})
	}
	for _, idx := range order {
		acc := accByIndex[idx]
		partial.Content = append(partial.Content, &ToolCallContent{
			ID: acc.id, Name: acc.name, Arguments: json.RawMessage(acc.args),
		})
		s.Push(Event{Type: EventToolCallEnd, Partial: partial})
	}

	switch finishReason {
	case "length", "max_tokens":
		partial.StopReason = StopLength
	case "tool_calls":
		partial.StopReason = StopToolUse
	default:
		partial.StopReason = StopStop
	}
	partial.Usage = usage
	s.Push(Event{Type: EventDone, Partial: partial})
	s.Finish(partial, nil)
	return nil
}
