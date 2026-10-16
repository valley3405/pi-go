package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// echoTool 是一个测试工具：原样返回 args 里的 text。
type echoTool struct{ calls *int32 }

func (t *echoTool) Name() string { return "echo" }
func (t *echoTool) Schema() llm.Tool {
	return llm.Tool{
		Name:        "echo",
		Description: "Echo text back",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
	}
}
func (t *echoTool) ExecutionMode() agent.ExecutionMode { return agent.ExecutionParallel }
func (t *echoTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	atomic.AddInt32(t.calls, 1)
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: "echo: " + p.Text}}}, nil
}

func newFauxAgent(script []llm.Message, toolList ...agent.AgentTool) (*agent.Agent, *llm.FauxProvider) {
	faux := llm.NewFauxProvider("faux", script, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{
		SystemPrompt: "test",
		Model:        model,
		Tools:        toolList,
		StreamFn:     reg.StreamSimple,
	})
	return a, faux
}

func TestSimplePrompt(t *testing.T) {
	a, _ := newFauxAgent([]llm.Message{llm.FauxAssistant("hello world")})

	var events []agent.EventType
	a.Subscribe(func(ev agent.Event) { events = append(events, ev.Type) })

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	want := []agent.EventType{
		agent.EventAgentStart, agent.EventTurnStart,
		agent.EventMessageStart, agent.EventMessageEnd, // user
		agent.EventMessageStart, // assistant
		// message_update 若干
		agent.EventMessageEnd,
		agent.EventTurnEnd,
		agent.EventAgentEnd,
	}
	assertEventSkeleton(t, events, want)

	msgs := a.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1].Text() != "hello world" {
		t.Fatalf("unexpected assistant text: %q", msgs[1].Text())
	}
	if msgs[1].StopReason != llm.StopStop {
		t.Fatalf("unexpected stop reason: %q", msgs[1].StopReason)
	}
}

// assertEventSkeleton 检查关键事件按序出现（忽略 message_update 数量）。
func assertEventSkeleton(t *testing.T, got []agent.EventType, want []agent.EventType) {
	t.Helper()
	filtered := make([]agent.EventType, 0, len(got))
	for _, e := range got {
		if e == agent.EventMessageUpdate {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) != len(want) {
		t.Fatalf("event skeleton mismatch:\n got %v\nwant %v", filtered, want)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Fatalf("event %d mismatch: got %v want %v", i, filtered, want)
		}
	}
}

func TestToolCallLoop(t *testing.T) {
	var calls int32
	tool := &echoTool{calls: &calls}
	a, faux := newFauxAgent([]llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"text":"ping"}`)}),
		llm.FauxAssistant("done"),
	}, tool)

	var toolStarted, toolEnded int32
	a.Subscribe(func(ev agent.Event) {
		switch ev.Type {
		case agent.EventToolExecutionStart:
			atomic.AddInt32(&toolStarted, 1)
		case agent.EventToolExecutionEnd:
			atomic.AddInt32(&toolEnded, 1)
		}
	})

	if err := a.Prompt(context.Background(), "echo ping"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("tool executed %d times, want 1", calls)
	}
	if toolStarted != 1 || toolEnded != 1 {
		t.Fatalf("tool events: started=%d ended=%d", toolStarted, toolEnded)
	}

	msgs := a.Messages()
	// user → assistant(toolCall) → toolResult → assistant(done)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[2].Role != llm.RoleToolResult || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("bad toolResult message: %+v", msgs[2].Message)
	}
	if msgs[2].Text() != "echo: ping" {
		t.Fatalf("unexpected tool result: %q", msgs[2].Text())
	}

	// 第二次 LLM 调用应包含 toolResult
	if len(faux.Requests) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(faux.Requests))
	}
	last := faux.Requests[1].Messages[len(faux.Requests[1].Messages)-1]
	if last.Role != llm.RoleToolResult {
		t.Fatalf("last message to LLM should be toolResult, got %s", last.Role)
	}
}

func TestParallelToolCalls(t *testing.T) {
	var calls int32
	tool := &echoTool{calls: &calls}
	a, _ := newFauxAgent([]llm.Message{
		llm.FauxToolCall("",
			&llm.ToolCallContent{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"a"}`)},
			&llm.ToolCallContent{ID: "c2", Name: "echo", Arguments: json.RawMessage(`{"text":"b"}`)},
		),
		llm.FauxAssistant("done"),
	}, tool)

	if err := a.Prompt(context.Background(), "two calls"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 tool executions, got %d", calls)
	}
	msgs := a.Messages()
	// 结果按原顺序入上下文
	if msgs[2].ToolCallID != "c1" || msgs[3].ToolCallID != "c2" {
		t.Fatalf("tool results out of order: %s, %s", msgs[2].ToolCallID, msgs[3].ToolCallID)
	}
}

func TestTruncatedMessageGuard(t *testing.T) {
	var calls int32
	tool := &echoTool{calls: &calls}
	truncated := llm.FauxToolCall("", &llm.ToolCallContent{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"a`)})
	truncated.StopReason = llm.StopLength

	a, _ := newFauxAgent([]llm.Message{truncated, llm.FauxAssistant("retry ok")}, tool)
	if err := a.Prompt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("truncated tool call must NOT be executed")
	}
	msgs := a.Messages()
	if msgs[2].Role != llm.RoleToolResult || !msgs[2].IsError {
		t.Fatalf("expected error toolResult for truncated call: %+v", msgs[2].Message)
	}
}

func TestBeforeToolCallBlock(t *testing.T) {
	var calls int32
	tool := &echoTool{calls: &calls}
	a, _ := newFauxAgent([]llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"a"}`)}),
		llm.FauxAssistant("blocked, ok"),
	}, tool)
	a.BeforeToolCall = func(ctx context.Context, c agent.BeforeToolCallContext) (*agent.BeforeToolCallResult, error) {
		return &agent.BeforeToolCallResult{Block: true, Reason: "not allowed"}, nil
	}

	if err := a.Prompt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("blocked tool must not execute")
	}
	if msgs := a.Messages(); msgs[2].Text() != "not allowed" {
		t.Fatalf("unexpected block reason: %q", msgs[2].Text())
	}
}

func TestSteeringQueue(t *testing.T) {
	a, faux := newFauxAgent([]llm.Message{
		llm.FauxAssistant("first"),
		llm.FauxAssistant("second"),
	})
	a.Subscribe(func(ev agent.Event) {
		// 在 assistant 第一条消息开始时注入 steering
		if ev.Type == agent.EventTurnEnd && len(a.Messages()) == 2 {
			a.Steer(agent.NewUserMessage("steered"))
		}
	})

	if err := a.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	msgs := a.Messages()
	// user, assistant(first), user(steered), assistant(second)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[2].Text() != "steered" {
		t.Fatalf("steering message missing: %q", msgs[2].Text())
	}
	if len(faux.Requests) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(faux.Requests))
	}
}

func TestFollowUpQueue(t *testing.T) {
	a, _ := newFauxAgent([]llm.Message{
		llm.FauxAssistant("first"),
		llm.FauxAssistant("after followup"),
	})
	a.FollowUp(agent.NewUserMessage("follow up!"))

	if err := a.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	msgs := a.Messages()
	if len(msgs) != 4 || msgs[2].Text() != "follow up!" {
		t.Fatalf("follow-up not consumed: %v", msgs)
	}
}

func TestAbort(t *testing.T) {
	// 脚本为空时 faux 立即返回；用一个慢 ctx 取消测 abort 路径
	a, _ := newFauxAgent(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	if err := a.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	// 只要不 panic 且 run 结束即通过
	if a.IsRunning() {
		t.Fatal("agent still running after abort")
	}
}

func TestPromptWhileRunning(t *testing.T) {
	block := make(chan struct{})
	slow := &slowTool{block: block}
	a, _ := newFauxAgent([]llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "c1", Name: "slow", Arguments: json.RawMessage(`{}`)}),
		llm.FauxAssistant("done"),
	}, slow)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// 等工具开始执行
	deadline := time.Now().Add(2 * time.Second)
	for !slow.started.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := a.Prompt(context.Background(), "again"); err == nil {
		t.Fatal("second prompt should fail while running")
	}
	close(block)
	a.WaitForIdle()
}

type slowTool struct {
	block   chan struct{}
	started atomic.Bool
}

func (t *slowTool) Name() string { return "slow" }
func (t *slowTool) Schema() llm.Tool {
	return llm.Tool{Name: "slow", Description: "slow", Parameters: map[string]any{"type": "object"}}
}
func (t *slowTool) ExecutionMode() agent.ExecutionMode { return agent.ExecutionParallel }
func (t *slowTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	t.started.Store(true)
	<-t.block
	return agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: "ok"}}}, nil
}

func TestConcurrentDoubleRun(t *testing.T) {
	a, _ := newFauxAgent([]llm.Message{llm.FauxAssistant("1"), llm.FauxAssistant("2")})
	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if err := a.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if n := len(a.Messages()); n != 4 {
		t.Fatalf("expected 4 messages, got %d", n)
	}
	fmt.Println("ok")
}

func TestBrokenStreamEmitsFinalMessage(t *testing.T) {
	a := agent.New(agent.Options{
		Model: llm.Model{ID: "broken", Provider: "broken"},
		StreamFn: func(ctx context.Context, model llm.Model, llmCtx llm.Context, opts llm.StreamOptions) *llm.EventStream {
			stream := llm.NewEventStream()
			partial := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{&llm.TextContent{Text: "partial"}}}
			stream.Push(llm.Event{Type: llm.EventStart, Partial: partial})
			stream.Finish(partial, fmt.Errorf("boom"))
			return stream
		},
	})
	var messageEnds, agentEnds int
	a.Subscribe(func(event agent.Event) {
		if event.Type == agent.EventMessageEnd && event.Message.Role == llm.RoleAssistant {
			messageEnds++
		}
		if event.Type == agent.EventAgentEnd {
			agentEnds++
		}
	})
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if messageEnds != 1 || agentEnds != 1 {
		t.Fatalf("terminal events: message_end=%d agent_end=%d", messageEnds, agentEnds)
	}
	messages := a.Messages()
	if len(messages) != 2 || messages[1].StopReason != llm.StopError {
		t.Fatalf("broken stream final message missing: %#v", messages)
	}
}

func TestCanceledHookEmitsAgentEnd(t *testing.T) {
	a, _ := newFauxAgent(nil)
	a.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) ([]agent.AgentMessage, error) {
		return nil, ctx.Err()
	}
	var agentEnds int
	a.Subscribe(func(event agent.Event) {
		if event.Type == agent.EventAgentEnd {
			agentEnds++
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if agentEnds != 1 {
		t.Fatalf("got %d agent_end events, want 1", agentEnds)
	}
	messages := a.Messages()
	if len(messages) != 2 || messages[1].StopReason != llm.StopAborted {
		t.Fatalf("canceled run final message missing: %#v", messages)
	}
}
