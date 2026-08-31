package agent

import (
	"encoding/json"

	"goagent/pkg/llm"
)

// AgentMessage 的 JSON 编解码。llm.Message 已带自定义编解码（内容块类型标签），
// 但嵌入字段的提升方法会丢掉 Custom/Payload，所以这里显式实现。

type agentMessageJSON struct {
	Message json.RawMessage `json:"message"`
	Custom  bool            `json:"custom,omitempty"`
	Payload any             `json:"payload,omitempty"`
}

// MarshalJSON 实现 AgentMessage 编码。
func (m AgentMessage) MarshalJSON() ([]byte, error) {
	mb, err := m.Message.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(agentMessageJSON{Message: mb, Custom: m.Custom, Payload: m.Payload})
}

// UnmarshalJSON 实现 AgentMessage 解码。
func (m *AgentMessage) UnmarshalJSON(data []byte) error {
	var amj agentMessageJSON
	if err := json.Unmarshal(data, &amj); err != nil {
		return err
	}
	var msg llm.Message
	if err := msg.UnmarshalJSON(amj.Message); err != nil {
		return err
	}
	m.Message = msg
	m.Custom = amj.Custom
	m.Payload = amj.Payload
	return nil
}
