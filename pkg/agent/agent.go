package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"goagent/pkg/llm"
	"goagent/pkg/permission"
)

// pendingMessageQueue 对应 pi 的 PendingMessageQueue。
type pendingMessageQueue struct {
	mu       sync.Mutex
	messages []AgentMessage
	Mode     QueueMode
}

func (q *pendingMessageQueue) enqueue(m AgentMessage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = append(q.messages, m)
}

func (q *pendingMessageQueue) hasItems() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages) > 0
}

func (q *pendingMessageQueue) drain() []AgentMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return nil
	}
	if q.Mode == QueueAll {
		out := q.messages
		q.messages = nil
		return out
	}
	first := q.messages[0]
	q.messages = q.messages[1:]
	return []AgentMessage{first}
}

func (q *pendingMessageQueue) clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = nil
}

// State 是 agent 的可读状态（对应 pi 的 AgentState）。
type State struct {
	SystemPrompt      string
	Model             llm.Model
	Messages          []AgentMessage
	IsStreaming       bool
	StreamingMessage  *AgentMessage
	PendingToolCalls  map[string]bool
	ErrorMessage      string
	TerminationReason TerminationReason
}

// Agent 是低层循环的有状态封装（对应 pi 的 Agent 类）：
// 持有 transcript、发出生命周期事件、执行工具、提供 steering/follow-up 队列。
type Agent struct {
	mu                 sync.Mutex
	state              State
	listeners          []listener
	nextLid            uint64
	tools              []AgentTool
	streamFn           llm.StreamFn
	streamOpts         llm.StreamOptions
	permission         permission.Evaluator
	maxTurns           int
	settlement         *SettlementConfig
	maxDuration        time.Duration
	toolOutputDir      string
	maxToolOutputChars int

	// 可替换管线（对应 pi 的公开字段）
	ConvertToLLM     func([]AgentMessage) ([]llm.Message, error)
	TransformContext func(context.Context, []AgentMessage) ([]AgentMessage, error)

	// 钩子
	BeforeToolCall      func(context.Context, BeforeToolCallContext) (*BeforeToolCallResult, error)
	AfterToolCall       func(context.Context, AfterToolCallContext) (*AfterToolCallResult, error)
	ShouldStopAfterTurn func(context.Context, ShouldStopAfterTurnContext) (bool, error)
	PrepareNextTurn     func(context.Context) (*TurnUpdate, error)
	GetAPIKey           func(provider string) string

	ToolExecution ExecutionMode

	steeringQueue *pendingMessageQueue
	followUpQueue *pendingMessageQueue

	// 当前 run
	runMu   sync.Mutex
	cancel  context.CancelFunc
	idleCh  chan struct{} // run 结束时 close
	running bool
}

// Options 是构造选项。
type Options struct {
	SystemPrompt  string
	Model         llm.Model
	Tools         []AgentTool
	StreamFn      llm.StreamFn // 必填；用 llm.Registry.StreamSimple 即可
	StreamOptions llm.StreamOptions
	SteeringMode  QueueMode
	FollowUpMode  QueueMode
	// Permission 为 nil 时普通工具不启用权限评估；ToolCallPreparer 工具仍使用默认策略。
	Permission permission.Evaluator
	// MaxTurns 限制每次 Prompt 的 LLM 响应次数；<=0 表示不限制。
	MaxTurns int
	// Settlement 非 nil 时，命中 MaxTurns 不立即终止：注入收尾提示、
	// 摘除指定工具并追加 Settlement.MaxTurns 回合；收尾预算再耗尽仍以 max_turns 结束。
	Settlement *SettlementConfig
	// MaxDuration 限制一次 Prompt 的总时长；<=0 表示不限制。
	MaxDuration time.Duration
	// ToolOutputDir 非空时，超限工具输出的完整文本保存到该目录。
	ToolOutputDir string
	// MaxToolOutputChars 限制返回给模型的工具输出字符数；<=0 使用默认值。
	MaxToolOutputChars int
}

// New 创建 Agent。
func New(opts Options) *Agent {
	if opts.SteeringMode == "" {
		opts.SteeringMode = QueueOneAtATime
	}
	if opts.FollowUpMode == "" {
		opts.FollowUpMode = QueueOneAtATime
	}
	a := &Agent{
		state:              State{SystemPrompt: opts.SystemPrompt, Model: opts.Model, PendingToolCalls: map[string]bool{}},
		tools:              opts.Tools,
		streamFn:           opts.StreamFn,
		streamOpts:         opts.StreamOptions,
		permission:         opts.Permission,
		maxTurns:           opts.MaxTurns,
		settlement:         opts.Settlement,
		maxDuration:        opts.MaxDuration,
		toolOutputDir:      opts.ToolOutputDir,
		maxToolOutputChars: opts.MaxToolOutputChars,
		steeringQueue:      &pendingMessageQueue{Mode: opts.SteeringMode},
		followUpQueue:      &pendingMessageQueue{Mode: opts.FollowUpMode},
		ToolExecution:      ExecutionParallel,
		idleCh:             closedCh(),
	}
	return a
}

func closedCh() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// listener 是带唯一 ID 的事件监听器（Go 函数值不可比较，用 ID 做删除标识）。
type listener struct {
	id uint64
	fn func(Event)
}

// Subscribe 注册事件监听器，返回取消函数。
// 监听器按注册顺序同步调用（对应 pi 的 await-in-order）。
func (a *Agent) Subscribe(fn func(Event)) (unsubscribe func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextLid++
	id := a.nextLid
	a.listeners = append(a.listeners, listener{id: id, fn: fn})
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, l := range a.listeners {
			if l.id == id {
				a.listeners = append(a.listeners[:i], a.listeners[i+1:]...)
				return
			}
		}
	}
}

// Snapshot 返回当前状态副本。
func (a *Agent) Snapshot() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	s.Messages = append([]AgentMessage{}, a.state.Messages...)
	return s
}

// Messages 返回 transcript 副本。
func (a *Agent) Messages() []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentMessage{}, a.state.Messages...)
}

// SetMessages 替换 transcript（用于会话恢复）。
func (a *Agent) SetMessages(msgs []AgentMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Messages = append([]AgentMessage{}, msgs...)
}

// SetTools 替换工具集。
func (a *Agent) SetTools(tools []AgentTool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = append([]AgentTool{}, tools...)
}

// Tools 返回当前工具集副本。
func (a *Agent) Tools() []AgentTool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentTool{}, a.tools...)
}

// SetModel 切换模型。
func (a *Agent) SetModel(m llm.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = m
}

// SetSystemPrompt 更新系统提示词。
func (a *Agent) SetSystemPrompt(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.SystemPrompt = s
}

// Steer 排队一条 steering 消息：当前回合结束后注入。
func (a *Agent) Steer(m AgentMessage) { a.steeringQueue.enqueue(m) }

// FollowUp 排队一条 follow-up 消息：agent 本要停止时才消费。
func (a *Agent) FollowUp(m AgentMessage) { a.followUpQueue.enqueue(m) }

// HasQueuedMessages 报告队列是否非空。
func (a *Agent) HasQueuedMessages() bool {
	return a.steeringQueue.hasItems() || a.followUpQueue.hasItems()
}

// ClearQueues 清空两个队列。
func (a *Agent) ClearQueues() {
	a.steeringQueue.clear()
	a.followUpQueue.clear()
}

// Abort 中止当前 run（对应 pi 的 abort()）。
func (a *Agent) Abort() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// WaitForIdle 阻塞直到当前 run（及其事件监听器）结束。
func (a *Agent) WaitForIdle() {
	a.runMu.Lock()
	ch := a.idleCh
	a.runMu.Unlock()
	<-ch
}

// IsRunning 报告是否有活动 run。
func (a *Agent) IsRunning() bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.running
}

// Reset 清空 transcript 与队列。运行中调用返回错误。
func (a *Agent) Reset() error {
	if a.IsRunning() {
		return errors.New("agent is already processing; wait for completion before resetting")
	}
	a.mu.Lock()
	a.state.Messages = nil
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.state.ErrorMessage = ""
	a.state.TerminationReason = ""
	a.mu.Unlock()
	a.ClearQueues()
	return nil
}

// Prompt 以一条文本消息启动新 run（对应 pi 的 prompt()）。
func (a *Agent) Prompt(ctx context.Context, text string) error {
	return a.PromptMessages(ctx, []AgentMessage{NewUserMessage(text)})
}

// PromptMessages 以一批消息启动新 run。
func (a *Agent) PromptMessages(ctx context.Context, messages []AgentMessage) error {
	if err := a.beginRun(); err != nil {
		return err
	}
	var runCtx context.Context
	var cancel context.CancelFunc
	if a.maxDuration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, a.maxDuration)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	a.runMu.Lock()
	a.cancel = cancel
	a.runMu.Unlock()

	go func() {
		defer cancel()
		_, reason, err := runAgentLoop(runCtx, messages, a.contextSnapshot(), a.buildLoopConfig(), a.streamFn, a.emit)
		a.finishRun(reason, err)
	}()
	return nil
}

// beginRun 占用 run 槽位；返回错误表示已有活动 run。
func (a *Agent) beginRun() error {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running {
		return errors.New("agent is already processing a prompt; use Steer()/FollowUp() or wait for completion")
	}
	a.running = true
	a.idleCh = make(chan struct{})
	a.mu.Lock()
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	a.state.TerminationReason = ""
	a.mu.Unlock()
	return nil
}

// finishRun 收尾：失败时合成错误消息并照常发事件（对应 pi 的 handleRunFailure），
// 保证 UI 状态机一致。
func (a *Agent) finishRun(reason TerminationReason, runErr error) {
	if runErr != nil {
		if reason == "" {
			reason = classifyRunError(runErr)
		}
		stopReason := llm.StopError
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			stopReason = llm.StopAborted
		}
		failMsg := AgentMessage{Message: llm.Message{
			Role:       llm.RoleAssistant,
			Content:    []llm.Content{&llm.TextContent{Text: ""}},
			StopReason: stopReason,
			Timestamp:  nowMs(),
		}}
		_ = a.emit(Event{Type: EventMessageStart, Message: &failMsg})
		_ = a.emit(Event{Type: EventMessageEnd, Message: &failMsg})
		_ = a.emit(Event{Type: EventTurnEnd, Message: &failMsg})
		_ = a.emit(Event{Type: EventAgentEnd, Reason: reason, Messages: []AgentMessage{failMsg}})
	}

	a.mu.Lock()
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]bool{}
	a.mu.Unlock()

	a.runMu.Lock()
	a.running = false
	a.cancel = nil
	close(a.idleCh)
	a.runMu.Unlock()
}

// contextSnapshot 对应 pi 的 createContextSnapshot()。
func (a *Agent) contextSnapshot() Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Context{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     append([]AgentMessage{}, a.state.Messages...),
		Tools:        append([]AgentTool{}, a.tools...),
	}
}

func (a *Agent) buildLoopConfig() loopConfig {
	return loopConfig{
		model:          a.state.Model,
		toolExecution:  a.ToolExecution,
		maxTurns:       a.maxTurns,
		settlement:     a.settlement,
		output:         outputConfig{dir: a.toolOutputDir, maxChars: a.maxToolOutputChars},
		permission:     a.permission,
		convertToLLM:   a.ConvertToLLM,
		transformCtx:   a.TransformContext,
		beforeToolCall: a.BeforeToolCall,
		afterToolCall:  a.AfterToolCall,
		shouldStop:     a.ShouldStopAfterTurn,
		prepareNext:    a.PrepareNextTurn,
		getSteering:    func(context.Context) []AgentMessage { return a.steeringQueue.drain() },
		getFollowUp:    func(context.Context) []AgentMessage { return a.followUpQueue.drain() },
		getAPIKey:      a.GetAPIKey,
		streamOpts:     a.streamOpts,
	}
}

// emit 先迁移内部状态（对应 pi 的 processEvents 的 reduce 部分），
// 再按序调用监听器。
func (a *Agent) emit(ev Event) error {
	a.mu.Lock()
	switch ev.Type {
	case EventMessageStart, EventMessageUpdate:
		if ev.Message != nil {
			m := *ev.Message
			a.state.StreamingMessage = &m
		}
	case EventMessageEnd:
		a.state.StreamingMessage = nil
		if ev.Message != nil {
			a.state.Messages = append(a.state.Messages, *ev.Message)
		}
	case EventToolExecutionStart:
		a.state.PendingToolCalls[ev.ToolCallID] = true
	case EventToolExecutionEnd:
		delete(a.state.PendingToolCalls, ev.ToolCallID)
	case EventTurnEnd:
		if ev.Message != nil && ev.Message.Role == llm.RoleAssistant && ev.Message.StopReason == llm.StopError {
			a.state.ErrorMessage = "agent run failed"
		}
	case EventAgentEnd:
		a.state.StreamingMessage = nil
		if ev.Reason == "" {
			ev.Reason = TerminationCompleted
		}
		a.state.TerminationReason = ev.Reason
	}
	listeners := make([]func(Event), 0, len(a.listeners))
	for _, l := range a.listeners {
		listeners = append(listeners, l.fn)
	}
	a.mu.Unlock()

	for _, l := range listeners {
		l(ev)
	}
	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }
