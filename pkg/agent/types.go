// Package agent 是有状态 agent 运行时（对应 pi 的 pi-agent-core）：
// 双层 agent 循环、steering/follow-up 双队列、生命周期事件、工具钩子。
// 本包不认识任何具体 LLM provider，只依赖 llm.StreamFn。
package agent

import (
	"context"
	"encoding/json"
	"time"

	"goagent/pkg/llm"
	"goagent/pkg/permission"
)

// AgentMessage 比 llm.Message 宽：允许携带 LLM 不认识的自定义/UI 消息
// （pi 用 declaration merging，Go 用显式 Custom 字段）。
type AgentMessage struct {
	llm.Message
	Custom  bool `json:"custom,omitempty"`
	Payload any  `json:"payload,omitempty"`
}

// NewUserMessage 构造 user 消息。
func NewUserMessage(text string) AgentMessage {
	return AgentMessage{Message: llm.NewUserMessage(text)}
}

// NewCustomMessage 构造一条只用于 UI/日志、不会发给 LLM 的消息。
func NewCustomMessage(payload any) AgentMessage {
	return AgentMessage{
		Message: llm.Message{Role: llm.RoleUser, Timestamp: time.Now().UnixMilli()},
		Custom:  true,
		Payload: payload,
	}
}

// ToolResult 是工具执行结果。
type ToolResult struct {
	Content   []llm.Content `json:"content"`
	Details   any           `json:"details,omitempty"`
	Terminate bool          `json:"terminate,omitempty"` // 整批结果都 Terminate 时终止循环
}

// ErrorToolResult 构造错误结果（对应 pi 的 createErrorToolResult）。
func ErrorToolResult(msg string) ToolResult {
	return ToolResult{Content: []llm.Content{&llm.TextContent{Text: msg}}}
}

// ExecutionMode 控制单条消息内多个工具调用的执行策略。
type ExecutionMode string

const (
	ExecutionParallel   ExecutionMode = "parallel"
	ExecutionSequential ExecutionMode = "sequential"
)

// AgentTool 是可被 LLM 调用的工具（对应 pi 的 AgentTool）。
type AgentTool interface {
	Name() string
	// Schema 返回发给 LLM 的工具描述（name/description/JSON Schema）。
	Schema() llm.Tool
	// ExecutionMode 默认 parallel；sequential 会强制整批串行。
	ExecutionMode() ExecutionMode
	// Execute 执行工具。onUpdate 可回调流式 partial result
	// （对应 pi 的 tool_execution_update）。
	Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(ToolResult)) (ToolResult, error)
}

// ReadOnlyTool 是可选接口；未实现时工具按非只读处理。
type ReadOnlyTool interface {
	IsReadOnly(args json.RawMessage) bool
}

// PermissionCheckedTool 是可选的工具权限自检接口。
type PermissionCheckedTool interface {
	CheckPermissions(ctx context.Context, args json.RawMessage) permission.Decision
}

// ToolCallPreparer 绑定一次调用的访问目标，避免审批后路径或符号链接改变授权范围。
// 返回的工具仅用于当前调用；即使未配置 Evaluator，也按默认权限策略检查。
type ToolCallPreparer interface {
	PrepareCall(ctx context.Context, args json.RawMessage) (AgentTool, error)
}

// TerminationReason 说明一次 agent run 的结束原因。
type TerminationReason string

const (
	TerminationCompleted     TerminationReason = "completed"
	TerminationAborted       TerminationReason = "aborted"
	TerminationTimeout       TerminationReason = "timeout"
	TerminationMaxTurns      TerminationReason = "max_turns"
	TerminationOutputLimit   TerminationReason = "output_limit"
	TerminationModelError    TerminationReason = "model_error"
	TerminationPromptTooLong TerminationReason = "prompt_too_long"
	TerminationError         TerminationReason = "error"
)

// ---- 生命周期事件（对应 pi 的 AgentEvent）----

// EventType 是 agent 生命周期事件类型。
type EventType string

const (
	EventAgentStart          EventType = "agent_start"
	EventAgentEnd            EventType = "agent_end"
	EventTurnStart           EventType = "turn_start"
	EventTurnEnd             EventType = "turn_end"
	EventMessageStart        EventType = "message_start"
	EventMessageUpdate       EventType = "message_update"
	EventMessageEnd          EventType = "message_end"
	EventToolExecutionStart  EventType = "tool_execution_start"
	EventToolExecutionUpdate EventType = "tool_execution_update"
	EventToolExecutionEnd    EventType = "tool_execution_end"
)

// Event 是 agent 生命周期事件。
type Event struct {
	Type    EventType
	Message *AgentMessage // message_* / turn_end 事件有效
	// 工具执行事件字段
	ToolCallID    string
	ToolName      string
	Args          json.RawMessage
	Result        *ToolResult
	IsError       bool
	PartialResult *ToolResult // tool_execution_update 有效
	// 流式更新
	StreamEvent *llm.Event // message_update 事件有效
	// turn_end / agent_end
	ToolResults []AgentMessage
	Messages    []AgentMessage // agent_end：本次 run 新增的全部消息
	Reason      TerminationReason
}

// ---- 钩子（对应 pi 的 beforeToolCall / afterToolCall / 等）----

// BeforeToolCallContext 传给 BeforeToolCall 钩子。
type BeforeToolCallContext struct {
	AssistantMessage AgentMessage
	ToolCall         llm.ToolCallContent
	Args             json.RawMessage // 已校验
}

// BeforeToolCallResult 非 nil 且 Block 时阻止执行（对应 pi 的 block/terminate）。
type BeforeToolCallResult struct {
	Block     bool
	Reason    string
	Terminate bool
}

// AfterToolCallContext 传给 AfterToolCall 钩子。
type AfterToolCallContext struct {
	AssistantMessage AgentMessage
	ToolCall         llm.ToolCallContent
	Result           ToolResult
	IsError          bool
}

// AfterToolCallResult 非 nil 时改写工具结果。
type AfterToolCallResult struct {
	Content   *[]llm.Content
	IsError   *bool
	Terminate *bool
}

// ShouldStopAfterTurnContext 传给 ShouldStopAfterTurn 钩子。
type ShouldStopAfterTurnContext struct {
	Message     AgentMessage
	ToolResults []AgentMessage
	NewMessages []AgentMessage
}

// TurnUpdate 是 PrepareNextTurn 可返回的下一回合调整（换模型/思考级别）。
type TurnUpdate struct {
	Model *llm.Model
}

// QueueMode 控制队列排空方式（对应 pi 的 QueueMode）。
type QueueMode string

const (
	QueueAll        QueueMode = "all"           // 一次排空全部
	QueueOneAtATime QueueMode = "one-at-a-time" // 一次取一条
)

// Context 是 agent 上下文的快照（对应 pi 的 AgentContext）。
type Context struct {
	SystemPrompt string
	Messages     []AgentMessage
	Tools        []AgentTool
}

// loopConfig 是传给循环的配置（对应 pi 的 AgentLoopConfig）。
type loopConfig struct {
	model          llm.Model
	toolExecution  ExecutionMode
	maxTurns       int
	output         outputConfig
	permission     permission.Evaluator
	convertToLLM   func([]AgentMessage) ([]llm.Message, error)
	transformCtx   func(context.Context, []AgentMessage) ([]AgentMessage, error)
	beforeToolCall func(context.Context, BeforeToolCallContext) (*BeforeToolCallResult, error)
	afterToolCall  func(context.Context, AfterToolCallContext) (*AfterToolCallResult, error)
	shouldStop     func(context.Context, ShouldStopAfterTurnContext) (bool, error)
	prepareNext    func(context.Context) (*TurnUpdate, error)
	getSteering    func(context.Context) []AgentMessage
	getFollowUp    func(context.Context) []AgentMessage
	getAPIKey      func(provider string) string
	streamOpts     llm.StreamOptions
}
