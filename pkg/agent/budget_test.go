package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valley3405/pi-go/pkg/agent"
	"github.com/valley3405/pi-go/pkg/llm"
)

func newBudgetAgent(script []llm.Message, maxTurns int, maxDuration time.Duration, toolList ...agent.AgentTool) (*agent.Agent, *llm.FauxProvider) {
	faux := llm.NewFauxProvider("faux", script, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{
		SystemPrompt: "test",
		Model:        model,
		Tools:        toolList,
		StreamFn:     reg.StreamSimple,
		MaxTurns:     maxTurns,
		MaxDuration:  maxDuration,
	})
	return a, faux
}

func waitForIdle(t *testing.T, a *agent.Agent) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		a.WaitForIdle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent to become idle")
	}
}

func TestMaxTurns(t *testing.T) {
	toolCall := func(id string) llm.Message {
		return llm.FauxToolCall("", &llm.ToolCallContent{ID: id, Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)})
	}
	var calls int32
	tool := &echoTool{calls: &calls}
	a, faux := newBudgetAgent([]llm.Message{toolCall("c1"), toolCall("c2"), toolCall("c3"), toolCall("c4")}, 2, 0, tool)
	var endReason agent.TerminationReason
	a.Subscribe(func(event agent.Event) {
		if event.Type == agent.EventAgentEnd {
			endReason = event.Reason
		}
	})
	if err := a.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if len(faux.Requests) != 2 || calls != 2 {
		t.Fatalf("requests=%d tool calls=%d, want 2", len(faux.Requests), calls)
	}
	if endReason != agent.TerminationMaxTurns || a.Snapshot().TerminationReason != agent.TerminationMaxTurns {
		t.Fatalf("event reason=%q state reason=%q", endReason, a.Snapshot().TerminationReason)
	}
}

func TestMaxDurationWhileStreaming(t *testing.T) {
	a := agent.New(agent.Options{
		Model:       llm.Model{ID: "stuck", Provider: "stuck"},
		MaxDuration: 30 * time.Millisecond,
		StreamFn: func(ctx context.Context, model llm.Model, llmCtx llm.Context, opts llm.StreamOptions) *llm.EventStream {
			stream := llm.NewEventStream()
			stream.Push(llm.Event{Type: llm.EventStart, Partial: llm.Message{Role: llm.RoleAssistant}})
			return stream
		},
	})
	if err := a.Prompt(context.Background(), "wait"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationTimeout {
		t.Fatalf("reason=%q, want timeout", reason)
	}
	messages := a.Messages()
	if len(messages) == 0 || messages[len(messages)-1].StopReason != llm.StopAborted {
		t.Fatalf("missing aborted terminal message: %#v", messages)
	}
}

type deadlineTool struct{ started chan struct{} }

func (t *deadlineTool) Name() string { return "deadline" }
func (t *deadlineTool) Schema() llm.Tool {
	return llm.Tool{Name: "deadline", Description: "wait for cancellation", Parameters: map[string]any{"type": "object"}}
}
func (t *deadlineTool) ExecutionMode() agent.ExecutionMode { return agent.ExecutionParallel }
func (t *deadlineTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	close(t.started)
	<-ctx.Done()
	return agent.ToolResult{}, ctx.Err()
}

func TestMaxDurationDuringTool(t *testing.T) {
	tool := &deadlineTool{started: make(chan struct{})}
	a, _ := newBudgetAgent([]llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "c1", Name: "deadline", Arguments: json.RawMessage(`{}`)}),
		llm.FauxAssistant("unreachable"),
	}, 0, 30*time.Millisecond, tool)
	if err := a.Prompt(context.Background(), "wait"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tool.started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationTimeout {
		t.Fatalf("reason=%q, want timeout", reason)
	}
	var results int
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleToolResult {
			results++
			if !msg.IsError {
				t.Fatalf("deadline tool result is not an error: %#v", msg)
			}
		}
	}
	if results != 1 {
		t.Fatalf("tool results=%d, want 1", results)
	}
}

func TestSettlementCompletesNaturally(t *testing.T) {
	toolCall := func(id string) llm.Message {
		return llm.FauxToolCall("", &llm.ToolCallContent{ID: id, Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)})
	}
	var calls int32
	faux := llm.NewFauxProvider("faux", []llm.Message{
		toolCall("c1"), toolCall("c2"), llm.FauxAssistant("wrap up done"),
	}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{
		SystemPrompt: "test",
		Model:        model,
		Tools:        []agent.AgentTool{&echoTool{calls: &calls}},
		StreamFn:     reg.StreamSimple,
		MaxTurns:     2,
		Settlement:   &agent.SettlementConfig{Prompt: "预算耗尽，请收尾", DisabledTools: []string{"echo"}, MaxTurns: 2},
	})
	if err := a.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationCompleted {
		t.Fatalf("reason=%q, want completed", reason)
	}
	if len(faux.Requests) != 3 || calls != 2 {
		t.Fatalf("requests=%d tool calls=%d, want 3 and 2", len(faux.Requests), calls)
	}
	// 收尾提示注入历史；前两个请求带 echo，收尾请求已摘除 echo
	var settlement int
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleUser && strings.Contains(msg.Text(), "预算耗尽") {
			settlement++
		}
	}
	if settlement != 1 {
		t.Fatalf("settlement prompts=%d, want 1", settlement)
	}
	for i, req := range faux.Requests {
		hasEcho := false
		for _, tool := range req.Tools {
			if tool.Name == "echo" {
				hasEcho = true
			}
		}
		if i < 2 && !hasEcho {
			t.Errorf("request %d should include echo", i)
		}
		if i == 2 && hasEcho {
			t.Errorf("settlement request should drop echo")
		}
	}
}

func TestSettlementBudgetExhausted(t *testing.T) {
	toolCall := func(id string) llm.Message {
		return llm.FauxToolCall("", &llm.ToolCallContent{ID: id, Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)})
	}
	var calls int32
	faux := llm.NewFauxProvider("faux", []llm.Message{toolCall("c1"), toolCall("c2"), toolCall("c3")}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{
		SystemPrompt: "test",
		Model:        model,
		Tools:        []agent.AgentTool{&echoTool{calls: &calls}},
		StreamFn:     reg.StreamSimple,
		MaxTurns:     1,
		Settlement:   &agent.SettlementConfig{Prompt: "预算耗尽，请收尾", DisabledTools: []string{"echo"}, MaxTurns: 1},
	})
	if err := a.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationMaxTurns {
		t.Fatalf("reason=%q, want max_turns", reason)
	}
	// 收尾阶段 echo 已摘除：不再执行，但 tool_use/tool_result 配对完整
	if calls != 1 {
		t.Fatalf("echo executions=%d, want 1 (disabled after settlement)", calls)
	}
	var toolUses, toolResults, notFound int
	for _, msg := range a.Messages() {
		toolUses += len(msg.ToolCalls())
		if msg.Role == llm.RoleToolResult {
			toolResults++
			if strings.Contains(msg.Text(), "not found") {
				notFound++
			}
		}
	}
	if toolUses != toolResults {
		t.Fatalf("tool uses=%d results=%d, pairing broken", toolUses, toolResults)
	}
	if notFound == 0 {
		t.Fatalf("disabled tool should produce not-found results")
	}
}

func TestStopLengthRecovery(t *testing.T) {
	truncated := llm.FauxAssistant("partial")
	truncated.StopReason = llm.StopLength
	a, faux := newBudgetAgent([]llm.Message{truncated, llm.FauxAssistant("complete")}, 0, 0)
	if err := a.Prompt(context.Background(), "write"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationCompleted {
		t.Fatalf("reason=%q, want completed", reason)
	}
	if len(faux.Requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(faux.Requests))
	}
	var recovery int
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleUser && strings.Contains(msg.Text(), "Resume directly") {
			recovery++
		}
	}
	if recovery != 1 {
		t.Fatalf("recovery messages=%d, want 1", recovery)
	}
}

func TestStopLengthRecoveryLimit(t *testing.T) {
	script := make([]llm.Message, 0, 5)
	for i := 0; i < 4; i++ {
		msg := llm.FauxAssistant("partial")
		msg.StopReason = llm.StopLength
		script = append(script, msg)
	}
	script = append(script, llm.FauxAssistant("unreachable"))
	a, faux := newBudgetAgent(script, 0, 0)
	if err := a.Prompt(context.Background(), "write"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	if reason := a.Snapshot().TerminationReason; reason != agent.TerminationOutputLimit {
		t.Fatalf("reason=%q, want output_limit", reason)
	}
	if len(faux.Requests) != 4 {
		t.Fatalf("requests=%d, want 4", len(faux.Requests))
	}
	var recovery int
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleUser && strings.Contains(msg.Text(), "Resume directly") {
			recovery++
		}
	}
	if recovery != 3 {
		t.Fatalf("recovery messages=%d, want 3", recovery)
	}
}

func TestProviderErrorTerminationReasons(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected agent.TerminationReason
	}{
		{name: "model error", err: errors.New("provider failed"), expected: agent.TerminationModelError},
		{name: "prompt too long", err: errors.New("openai: HTTP 413: prompt is too long"), expected: agent.TerminationPromptTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := agent.New(agent.Options{
				Model: llm.Model{ID: "broken", Provider: "broken"},
				StreamFn: func(ctx context.Context, model llm.Model, llmCtx llm.Context, opts llm.StreamOptions) *llm.EventStream {
					stream := llm.NewEventStream()
					msg := llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopError}
					stream.Push(llm.Event{Type: llm.EventStart, Partial: msg})
					stream.Finish(msg, tt.err)
					return stream
				},
			})
			if err := a.Prompt(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			waitForIdle(t, a)
			if reason := a.Snapshot().TerminationReason; reason != tt.expected {
				t.Fatalf("reason=%q, want %q", reason, tt.expected)
			}
		})
	}
}
