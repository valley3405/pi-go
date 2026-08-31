package llm

import (
	"encoding/json"
	"fmt"
)

// Content 接口的 JSON 编解码：写入 "type" 标签字段以便反序列化
// 回具体类型。会话持久化（JSONL）依赖它。

type contentJSON struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Data      string          `json:"data,omitempty"`
	MimeType  string          `json:"mimeType,omitempty"`
}

// MarshalContent 编码单个内容块。
func MarshalContent(c Content) ([]byte, error) {
	switch v := c.(type) {
	case *TextContent:
		return json.Marshal(contentJSON{Type: "text", Text: v.Text})
	case *ThinkingContent:
		return json.Marshal(contentJSON{Type: "thinking", Thinking: v.Thinking})
	case *ToolCallContent:
		return json.Marshal(contentJSON{Type: "toolCall", ID: v.ID, Name: v.Name, Arguments: v.Arguments})
	case *ImageContent:
		return json.Marshal(contentJSON{Type: "image", Data: v.Data, MimeType: v.MimeType})
	}
	return nil, fmt.Errorf("unknown content type %T", c)
}

// UnmarshalContent 解码单个内容块。
func UnmarshalContent(data []byte) (Content, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	var cj contentJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return nil, err
	}
	switch head.Type {
	case "text":
		return &TextContent{Text: cj.Text}, nil
	case "thinking":
		return &ThinkingContent{Thinking: cj.Thinking}, nil
	case "toolCall":
		return &ToolCallContent{ID: cj.ID, Name: cj.Name, Arguments: cj.Arguments}, nil
	case "image":
		return &ImageContent{Data: cj.Data, MimeType: cj.MimeType}, nil
	}
	return nil, fmt.Errorf("unknown content type %q", head.Type)
}

// messageJSON 是 Message 的 JSON 表示。
type messageJSON struct {
	Role       Role              `json:"role"`
	Content    []json.RawMessage `json:"content"`
	StopReason StopReason        `json:"stopReason,omitempty"`
	Usage      Usage             `json:"usage,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	ToolName   string            `json:"toolName,omitempty"`
	IsError    bool              `json:"isError,omitempty"`
	Timestamp  int64             `json:"timestamp"`
}

// MarshalJSON 实现 Message 的自定义编码。
func (m Message) MarshalJSON() ([]byte, error) {
	mj := messageJSON{
		Role: m.Role, StopReason: m.StopReason, Usage: m.Usage,
		ToolCallID: m.ToolCallID, ToolName: m.ToolName, IsError: m.IsError, Timestamp: m.Timestamp,
	}
	for _, c := range m.Content {
		b, err := MarshalContent(c)
		if err != nil {
			return nil, err
		}
		mj.Content = append(mj.Content, b)
	}
	if mj.Content == nil {
		mj.Content = []json.RawMessage{}
	}
	return json.Marshal(mj)
}

// UnmarshalJSON 实现 Message 的自定义解码。
func (m *Message) UnmarshalJSON(data []byte) error {
	var mj messageJSON
	if err := json.Unmarshal(data, &mj); err != nil {
		return err
	}
	m.Role, m.StopReason, m.Usage = mj.Role, mj.StopReason, mj.Usage
	m.ToolCallID, m.ToolName, m.IsError, m.Timestamp = mj.ToolCallID, mj.ToolName, mj.IsError, mj.Timestamp
	m.Content = nil
	for _, raw := range mj.Content {
		c, err := UnmarshalContent(raw)
		if err != nil {
			return err
		}
		m.Content = append(m.Content, c)
	}
	return nil
}
