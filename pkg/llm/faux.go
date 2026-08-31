package llm

import (
	"context"
	"strings"
)

// FauxProvider 是脚本化假模型（对应 pi 的 faux provider），用于无 API key 的测试。
// 每次调用按脚本顺序弹出一条响应；文本部分按 chunk 大小拆成 delta 事件模拟流式。
type FauxProvider struct {
	id        string
	models    []Model
	script    []Message // 依次返回的完整 assistant 消息
	chunkSize int
	// Requests 记录每次调用收到的 Context，供断言。
	Requests []Context
}

// NewFauxProvider 创建假 provider。chunkSize<=0 时默认 8。
func NewFauxProvider(id string, script []Message, chunkSize int) *FauxProvider {
	if chunkSize <= 0 {
		chunkSize = 8
	}
	return &FauxProvider{
		id:        id,
		models:    []Model{{ID: "faux-1", Name: "Faux", Provider: id, ContextWindow: 128000, MaxTokens: 8192}},
		script:    script,
		chunkSize: chunkSize,
	}
}

func (p *FauxProvider) ID() string      { return p.id }
func (p *FauxProvider) Models() []Model { return p.models }

// AddResponse 追加一条脚本响应。
func (p *FauxProvider) AddResponse(m Message) { p.script = append(p.script, m) }

func (p *FauxProvider) Stream(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream {
	s := NewEventStream()
	go func() {
		p.Requests = append(p.Requests, c)

		if len(p.script) == 0 {
			msg := Message{Role: RoleAssistant, Content: []Content{&TextContent{Text: "(faux: script exhausted)"}}, StopReason: StopStop, Timestamp: nowMs()}
			s.Push(Event{Type: EventStart, Partial: msg})
			s.Push(Event{Type: EventDone, Partial: msg})
			s.Finish(msg, nil)
			return
		}
		msg := p.script[0]
		p.script = p.script[1:]
		msg.Timestamp = nowMs()

		partial := Message{Role: RoleAssistant, Timestamp: msg.Timestamp}
		s.Push(Event{Type: EventStart, Partial: partial})

		for _, content := range msg.Content {
			if ctx.Err() != nil {
				partial.StopReason = StopAborted
				s.Push(Event{Type: EventDone, Partial: partial})
				s.Finish(partial, nil)
				return
			}
			switch tc := content.(type) {
			case *TextContent:
				s.Push(Event{Type: EventTextStart, Partial: partial})
				cur := ""
				for _, chunk := range chunkString(tc.Text, p.chunkSize) {
					cur += chunk
					pc := cloneWith(partial, &TextContent{Text: cur})
					s.Push(Event{Type: EventTextDelta, Delta: chunk, Partial: pc})
				}
				partial.Content = append(partial.Content, &TextContent{Text: tc.Text})
				s.Push(Event{Type: EventTextEnd, Partial: partial})
			case *ThinkingContent:
				s.Push(Event{Type: EventThinkingStart, Partial: partial})
				s.Push(Event{Type: EventThinkingDelta, Delta: tc.Thinking, Partial: cloneWith(partial, tc)})
				partial.Content = append(partial.Content, tc)
				s.Push(Event{Type: EventThinkingEnd, Partial: partial})
			case *ToolCallContent:
				s.Push(Event{Type: EventToolCallStart, Partial: partial})
				args := string(tc.Arguments)
				cur := ""
				for _, chunk := range chunkString(args, p.chunkSize) {
					cur += chunk
					s.Push(Event{Type: EventToolCallDelta, Delta: chunk, Partial: partial})
				}
				partial.Content = append(partial.Content, tc)
				s.Push(Event{Type: EventToolCallEnd, Partial: partial})
			default:
				partial.Content = append(partial.Content, content)
			}
		}
		partial.StopReason = msg.StopReason
		partial.Usage = msg.Usage
		if partial.StopReason == "" {
			partial.StopReason = StopStop
		}
		s.Push(Event{Type: EventDone, Partial: partial})
		s.Finish(partial, nil)
	}()
	return s
}

func chunkString(s string, n int) []string {
	if s == "" {
		return nil
	}
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i += n {
		j := i + n
		if j > len(rs) {
			j = len(rs)
		}
		out = append(out, string(rs[i:j]))
	}
	return out
}

func cloneWith(base Message, extra Content) Message {
	m := base
	m.Content = append(append([]Content{}, base.Content...), extra)
	return m
}

// FauxAssistant 便捷构造脚本里的 assistant 文本消息。
func FauxAssistant(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{&TextContent{Text: text}}, StopReason: StopStop}
}

// FauxToolCall 便捷构造脚本里带工具调用的 assistant 消息。
func FauxToolCall(text string, calls ...*ToolCallContent) Message {
	var content []Content
	if strings.TrimSpace(text) != "" {
		content = append(content, &TextContent{Text: text})
	}
	for _, c := range calls {
		content = append(content, c)
	}
	return Message{Role: RoleAssistant, Content: content, StopReason: StopToolUse}
}
