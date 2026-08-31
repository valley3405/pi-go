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

// AnthropicProvider 实现 Anthropic Messages API（SSE 流式，
// content_block_* 事件映射到统一事件模型）。
type AnthropicProvider struct {
	id      string
	baseURL string
	models  []Model
	client  *http.Client
}

// NewAnthropicProvider 创建 Anthropic provider。
func NewAnthropicProvider(id string, models []Model) *AnthropicProvider {
	for i := range models {
		models[i].Provider = id
		models[i].BaseURL = "https://api.anthropic.com"
	}
	return &AnthropicProvider{id: id, baseURL: "https://api.anthropic.com", models: models, client: &http.Client{}}
}

func (p *AnthropicProvider) ID() string      { return p.id }
func (p *AnthropicProvider) Models() []Model { return p.models }

// ---- Anthropic wire 格式 ----

type antRequest struct {
	Model     string       `json:"model"`
	System    string       `json:"system,omitempty"`
	Messages  []antMessage `json:"messages"`
	Tools     []antTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens"`
	Stream    bool         `json:"stream"`
}

type antMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type antTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

func toAntMessages(c Context) []antMessage {
	var out []antMessage
	for _, m := range c.Messages {
		switch m.Role {
		case RoleUser:
			var parts []any
			for _, cc := range m.Content {
				switch v := cc.(type) {
				case *TextContent:
					parts = append(parts, map[string]any{"type": "text", "text": v.Text})
				case *ImageContent:
					parts = append(parts, map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "base64", "media_type": v.MimeType, "data": v.Data},
					})
				}
			}
			out = append(out, antMessage{Role: "user", Content: parts})
		case RoleAssistant:
			var parts []any
			for _, cc := range m.Content {
				switch v := cc.(type) {
				case *TextContent:
					parts = append(parts, map[string]any{"type": "text", "text": v.Text})
				case *ThinkingContent:
					parts = append(parts, map[string]any{"type": "thinking", "thinking": v.Thinking, "signature": ""})
				case *ToolCallContent:
					var input map[string]any
					_ = json.Unmarshal(v.Arguments, &input)
					parts = append(parts, map[string]any{
						"type": "tool_use", "id": v.ID, "name": v.Name, "input": input,
					})
				}
			}
			out = append(out, antMessage{Role: "assistant", Content: parts})
		case RoleToolResult:
			var text string
			for _, cc := range m.Content {
				if t, ok := cc.(*TextContent); ok {
					text += t.Text
				}
			}
			out = append(out, antMessage{Role: "user", Content: []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": m.ToolCallID,
					"content": text, "is_error": m.IsError,
				},
			}})
		}
	}
	return out
}

func (p *AnthropicProvider) Stream(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream {
	s := NewEventStream()
	go func() {
		if err := p.stream(ctx, model, c, opts, s); err != nil {
			msg := Message{Role: RoleAssistant, StopReason: StopError, Timestamp: nowMs()}
			s.Finish(msg, err)
		}
	}()
	return s
}

func (p *AnthropicProvider) stream(ctx context.Context, model Model, c Context, opts StreamOptions, s *EventStream) error {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	reqBody := antRequest{
		Model:     model.ID,
		System:    c.SystemPrompt,
		Messages:  toAntMessages(c),
		MaxTokens: maxTokens,
		Stream:    true,
	}
	for _, t := range c.Tools {
		reqBody.Tools = append(reqBody.Tools, antTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if opts.APIKey != "" {
		req.Header.Set("x-api-key", opts.APIKey)
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
		return fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return consumeAntSSE(resp.Body, s)
}

// ---- SSE 消费 ----

type antEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func consumeAntSSE(r io.Reader, s *EventStream) error {
	partial := Message{Role: RoleAssistant, Timestamp: nowMs()}
	s.Push(Event{Type: EventStart, Partial: partial})

	// 按 content block index 跟踪：kind / 累积文本 / 工具调用
	type block struct {
		kind     string // text / thinking / tool_use
		text     string
		id, name string
		argsJSON string
	}
	blocks := map[int]*block{}
	stopReason := ""
	usage := Usage{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev antEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Usage != nil {
				usage.Input = ev.Usage.InputTokens
			}
		case "content_block_start":
			b := &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			blocks[ev.Index] = b
			switch b.kind {
			case "text":
				s.Push(Event{Type: EventTextStart, Partial: partial})
			case "thinking":
				s.Push(Event{Type: EventThinkingStart, Partial: partial})
			case "tool_use":
				s.Push(Event{Type: EventToolCallStart, Partial: partial})
			}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text += ev.Delta.Text
				p := partial
				p.Content = append(append([]Content{}, partial.Content...), &TextContent{Text: b.text})
				s.Push(Event{Type: EventTextDelta, Delta: ev.Delta.Text, Partial: p})
			case "thinking_delta":
				b.text += ev.Delta.Thinking
				s.Push(Event{Type: EventThinkingDelta, Delta: ev.Delta.Thinking, Partial: partial})
			case "input_json_delta":
				b.argsJSON += ev.Delta.PartialJSON
				s.Push(Event{Type: EventToolCallDelta, Delta: ev.Delta.PartialJSON, Partial: partial})
			}
		case "content_block_stop":
			b := blocks[ev.Index]
			if b == nil {
				continue
			}
			switch b.kind {
			case "text":
				partial.Content = append(partial.Content, &TextContent{Text: b.text})
				s.Push(Event{Type: EventTextEnd, Partial: partial})
			case "thinking":
				partial.Content = append(partial.Content, &ThinkingContent{Thinking: b.text})
				s.Push(Event{Type: EventThinkingEnd, Partial: partial})
			case "tool_use":
				args := json.RawMessage(b.argsJSON)
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				partial.Content = append(partial.Content, &ToolCallContent{ID: b.id, Name: b.name, Arguments: args})
				s.Push(Event{Type: EventToolCallEnd, Partial: partial})
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.Output = ev.Usage.OutputTokens
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	switch stopReason {
	case "max_tokens":
		partial.StopReason = StopLength
	case "tool_use":
		partial.StopReason = StopToolUse
	default:
		partial.StopReason = StopStop
	}
	usage.TotalTokens = usage.Input + usage.Output
	partial.Usage = usage
	s.Push(Event{Type: EventDone, Partial: partial})
	s.Finish(partial, nil)
	return nil
}
