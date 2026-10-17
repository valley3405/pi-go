package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/llm"
	"goagent/pkg/tools"
)

func newTestHarness(t *testing.T, script []llm.Message, withSession bool, toolList ...agent.AgentTool) *harness.Harness {
	t.Helper()
	faux := llm.NewFauxProvider("faux", script, 3)
	registry := llm.NewRegistry()
	registry.SetProvider(faux)
	model, err := registry.GetModel("faux", "faux-1")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Options{
		SystemPrompt: "test",
		Model:        model,
		Tools:        toolList,
		StreamFn:     registry.StreamSimple,
	})
	var session *harness.Session
	if withSession {
		session, err = harness.CreateSession(t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
	}
	return harness.New(a, session, registry.StreamSimple, llm.StreamOptions{})
}

func updateModel(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	model, ok := updated.(Model)
	if !ok {
		t.Fatalf("unexpected model type %T", updated)
	}
	return model, cmd
}

func submitText(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m.input.SetValue(text)
	return updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func applyAgentEvent(t *testing.T, m Model, event agent.Event) Model {
	t.Helper()
	updated, cmd := updateModel(t, m, agentEventMsg{event: event})
	if cmd != nil {
		t.Fatal("agent event returned an unexpected command")
	}
	return updated
}

func runPromptAndDrain(t *testing.T, m Model, h *harness.Harness, events <-chan agent.Event, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("prompt did not return a command")
	}
	updated, next := updateModel(t, m, cmd())
	if next != nil {
		t.Fatal("prompt completion returned an unexpected follow-up command")
	}
	return drainAgent(t, updated, h, events)
}

func drainAgent(t *testing.T, m Model, h *harness.Harness, events <-chan agent.Event) Model {
	t.Helper()
	idle := make(chan struct{})
	go func() {
		h.Agent.WaitForIdle()
		close(idle)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			m = applyAgentEvent(t, m, event)
		case <-idle:
			for {
				select {
				case event := <-events:
					m = applyAgentEvent(t, m, event)
				default:
					return m
				}
			}
		case <-timer.C:
			t.Fatal("timed out waiting for agent")
		}
	}
}

func TestPromptStreamsToolsAndRenders(t *testing.T) {
	toolResponse := llm.FauxToolCall("working", &llm.ToolCallContent{
		ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"printf tool-output"}`),
	})
	toolResponse.Usage = llm.Usage{Input: 100, Output: 20, CacheRead: 40}
	finalResponse := llm.FauxAssistant("done")
	finalResponse.Usage = llm.Usage{Input: 130, Output: 30, TotalTokens: 160}
	h := newTestHarness(t, []llm.Message{toolResponse, finalResponse}, true, tools.Bash())
	m := New(h)
	events := make(chan agent.Event, 1024)
	unsubscribe := h.Agent.Subscribe(func(event agent.Event) { events <- event })
	t.Cleanup(unsubscribe)

	m, cmd := submitText(t, m, "run the tool")
	if !m.busy {
		t.Fatal("submit should mark the model busy")
	}
	m = runPromptAndDrain(t, m, h, events, cmd)

	if m.busy {
		t.Fatal("agent completion should clear busy state")
	}
	if len(m.entries) != 4 {
		t.Fatalf("got %d transcript entries, want 4: %#v", len(m.entries), m.entries)
	}
	if m.entries[0].kind != entryUser || m.entries[0].text != "run the tool" {
		t.Fatalf("unexpected user entry: %#v", m.entries[0])
	}
	if m.entries[1].kind != entryAssistant || m.entries[1].text != "working" {
		t.Fatalf("unexpected first assistant entry: %#v", m.entries[1])
	}
	toolEntry := m.entries[2]
	if toolEntry.kind != entryTool || toolEntry.callID != "call-1" || toolEntry.output != "tool-output" || toolEntry.running || toolEntry.failed {
		t.Fatalf("unexpected tool entry: %#v", toolEntry)
	}
	if m.entries[3].text != "done" {
		t.Fatalf("unexpected final assistant entry: %#v", m.entries[3])
	}
	view := m.View()
	for _, want := range []string{"run the tool", "working", "bash", "tool-output", "done", "tokens in 130 out 30 · total 280"} {
		if !strings.Contains(view, want) {
			t.Errorf("view does not contain %q:\n%s", want, view)
		}
	}
	if got := len(h.Agent.Messages()); got != 4 {
		t.Fatalf("agent has %d messages, want 4", got)
	}
	if m.usage.responses != 2 || m.usage.last != finalResponse.Usage || m.usage.total.Input != 230 || m.usage.total.Output != 50 || m.usage.total.Total() != 280 || m.usage.total.CacheRead != 40 {
		t.Fatalf("unexpected usage aggregate: %+v", m.usage)
	}

	m.rebuildTranscript()
	if m.usage.responses != 2 || m.usage.total.Total() != 280 {
		t.Fatalf("transcript rebuild duplicated usage: %+v", m.usage)
	}
	m, cmd = submitText(t, m, "/usage")
	if cmd != nil {
		t.Fatal("/usage returned unexpected async work")
	}
	detail := m.entries[len(m.entries)-1].text
	for _, want := range []string{"最近响应：input 130 · output 30 · total 160", "累计（2 次 assistant 响应）：input 230 · output 50 · total 280 · cacheRead 40"} {
		if !strings.Contains(detail, want) {
			t.Errorf("/usage detail does not contain %q:\n%s", want, detail)
		}
	}
}

func TestUsageCountsBlankAssistant(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	message := agent.AgentMessage{Message: llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.Content{&llm.ToolCallContent{
			ID: "usage-call", Name: "read", Arguments: json.RawMessage(`{"path":"go.mod"}`),
		}},
		Usage: llm.Usage{Input: 80, Output: 20, CacheWrite: 5},
	}}
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventMessageStart, Message: &message})
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventMessageEnd, Message: &message})
	if len(m.entries) != 0 {
		t.Fatalf("blank tool-only assistant should not render: %#v", m.entries)
	}
	if m.usage.responses != 1 || m.usage.total.Total() != 100 || m.usage.total.CacheWrite != 5 {
		t.Fatalf("tool-only assistant usage was not counted: %+v", m.usage)
	}
}

func TestUsageCommandWithoutData(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	m, cmd := submitText(t, m, "/usage")
	if cmd != nil {
		t.Fatal("/usage returned unexpected async work")
	}
	if !strings.Contains(m.entries[len(m.entries)-1].text, "暂无 token 用量数据") {
		t.Fatalf("unexpected empty usage detail: %#v", m.entries[len(m.entries)-1])
	}
	if strings.Contains(m.statusView(), "tokens in") {
		t.Fatalf("empty usage rendered in status: %q", m.statusView())
	}
}

func TestBrokenStreamIsVisible(t *testing.T) {
	a := agent.New(agent.Options{
		Model: llm.Model{ID: "broken", Provider: "broken"},
		StreamFn: func(ctx context.Context, model llm.Model, llmCtx llm.Context, opts llm.StreamOptions) *llm.EventStream {
			stream := llm.NewEventStream()
			partial := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{&llm.TextContent{Text: "partial"}}}
			stream.Push(llm.Event{Type: llm.EventStart, Partial: partial})
			stream.Finish(partial, errors.New("provider failed"))
			return stream
		},
	})
	h := harness.New(a, nil, nil, llm.StreamOptions{})
	m := New(h)
	events := make(chan agent.Event, 128)
	unsubscribe := h.Agent.Subscribe(func(event agent.Event) { events <- event })
	t.Cleanup(unsubscribe)

	m, cmd := submitText(t, m, "hi")
	m = runPromptAndDrain(t, m, h, events, cmd)
	if m.busy {
		t.Fatal("broken stream left model busy")
	}
	last := m.entries[len(m.entries)-1]
	if last.kind != entryAssistant || !last.failed || last.stopReason != llm.StopError {
		t.Fatalf("broken stream final entry: %#v", last)
	}
	if view := m.View(); !strings.Contains(view, "partial") || !strings.Contains(view, "agent run ended: error") {
		t.Fatalf("broken stream error not visible:\n%s", view)
	}
}

func TestCanceledHookClearsBusy(t *testing.T) {
	h := newTestHarness(t, nil, false)
	h.Agent.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) ([]agent.AgentMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := New(h)
	events := make(chan agent.Event, 128)
	unsubscribe := h.Agent.Subscribe(func(event agent.Event) { events <- event })
	t.Cleanup(unsubscribe)

	m, cmd := submitText(t, m, "cancel me")
	updated, next := updateModel(t, m, cmd())
	if next != nil {
		t.Fatal("prompt completion returned an unexpected command")
	}
	m, _ = updateModel(t, updated, tea.KeyMsg{Type: tea.KeyCtrlC})
	m = drainAgent(t, m, h, events)
	if m.busy {
		t.Fatal("canceled hook left model busy")
	}
	last := m.entries[len(m.entries)-1]
	if last.stopReason != llm.StopAborted {
		t.Fatalf("canceled hook final entry: %#v", last)
	}
}

func TestParallelToolEventsUseCallID(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	m.applyAgentEvent(agent.Event{Type: agent.EventToolExecutionStart, ToolCallID: "c1", ToolName: "read", Args: json.RawMessage(`{"path":"a"}`)})
	m.applyAgentEvent(agent.Event{Type: agent.EventToolExecutionStart, ToolCallID: "c2", ToolName: "read", Args: json.RawMessage(`{"path":"b"}`)})
	m.applyAgentEvent(agent.Event{Type: agent.EventToolExecutionUpdate, ToolCallID: "c2", ToolName: "read", PartialResult: &agent.ToolResult{
		Content: []llm.Content{&llm.TextContent{Text: "partial-b"}},
	}})
	m.applyAgentEvent(agent.Event{Type: agent.EventToolExecutionEnd, ToolCallID: "c1", ToolName: "read", IsError: true, Result: &agent.ToolResult{
		Content: []llm.Content{&llm.TextContent{Text: "final-a"}},
	}})

	if len(m.entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(m.entries))
	}
	if m.entries[0].callID != "c1" || m.entries[0].output != "final-a" || !m.entries[0].failed || m.entries[0].running {
		t.Errorf("wrong c1 entry: %#v", m.entries[0])
	}
	if m.entries[1].callID != "c2" || m.entries[1].output != "partial-b" || m.entries[1].failed || !m.entries[1].running {
		t.Errorf("wrong c2 entry: %#v", m.entries[1])
	}
}

func TestMultilineInputAndAbort(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	m.input.SetValue("first")
	updated, _ := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = updated
	if got := m.input.Value(); got != "first\n" {
		t.Fatalf("alt+enter produced %q, want a newline", got)
	}
	if len(m.entries) != 0 {
		t.Fatal("newline should not submit a prompt")
	}

	ctx, done := m.runtime.begin()
	m.busy = true
	updated, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated
	select {
	case <-ctx.Done():
	default:
		t.Fatal("ctrl+c did not cancel the active operation")
	}
	if !m.busy || m.notice == "" {
		t.Fatalf("abort should keep busy state until completion and show notice: %#v", m)
	}
	m.applyAgentEvent(agent.Event{Type: agent.EventAgentEnd})
	if m.busy {
		t.Fatal("agent end did not clear busy state")
	}
	done()
}

func TestSlashCommandDoesNotReachModel(t *testing.T) {
	h := newTestHarness(t, nil, false)
	m := New(h)
	m, cmd := submitText(t, m, "/not-a-command")
	if cmd != nil {
		t.Fatal("unknown slash command should not start async work")
	}
	if len(h.Agent.Messages()) != 0 {
		t.Fatal("slash command was sent to the model")
	}
	last := m.entries[len(m.entries)-1]
	if last.kind != entrySystem || !last.failed || !strings.Contains(last.text, "未知命令") {
		t.Fatalf("unexpected error entry: %#v", last)
	}
}

func TestUserBashAndCompaction(t *testing.T) {
	h := newTestHarness(t, []llm.Message{llm.FauxAssistant("compact-summary")}, true)
	for _, message := range []string{"one", "two", "three", "four", "five"} {
		if _, err := h.Session.AppendMessage(agent.NewUserMessage(message)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Resume(); err != nil {
		t.Fatal(err)
	}
	h.KeepRecent = 2
	m := New(h)

	m, cmd := submitText(t, m, "!printf shell-output")
	if cmd == nil {
		t.Fatal("bash command did not return async work")
	}
	updated, _ := updateModel(t, m, cmd())
	m = updated
	if m.busy || !strings.Contains(m.View(), "shell-output") {
		t.Fatalf("bash result missing or still busy:\n%s", m.View())
	}

	m, cmd = submitText(t, m, "!exit 7")
	m.notice = "正在中止当前任务…"
	updated, _ = updateModel(t, m, cmd())
	m = updated
	lastTool := m.entries[len(m.entries)-1]
	if !lastTool.failed || !strings.Contains(lastTool.output, "exit code 7") {
		t.Fatalf("nonzero bash exit not marked failed: %#v", lastTool)
	}
	if m.notice != "" {
		t.Fatalf("completion left stale notice %q", m.notice)
	}

	m, cmd = submitText(t, m, "/compact")
	if cmd == nil {
		t.Fatal("compact did not return async work")
	}
	updated, _ = updateModel(t, m, cmd())
	m = updated
	if m.busy {
		t.Fatal("compact left model busy")
	}
	if !strings.Contains(m.View(), "compact-summary") || !strings.Contains(m.View(), "上下文已压缩") {
		t.Fatalf("compacted transcript not rebuilt:\n%s", m.View())
	}
}

func TestBranchAndResumeRebuildTranscript(t *testing.T) {
	h := newTestHarness(t, nil, true)
	firstID, err := h.Session.AppendMessage(agent.NewUserMessage("first"))
	if err != nil {
		t.Fatal(err)
	}
	later := llm.FauxAssistant("later")
	later.Usage = llm.Usage{Input: 50, Output: 10}
	if _, err := h.Session.AppendMessage(agent.AgentMessage{Message: later}); err != nil {
		t.Fatal(err)
	}
	if err := h.Resume(); err != nil {
		t.Fatal(err)
	}
	m := New(h)
	if len(m.entries) != 2 {
		t.Fatalf("initial rebuild got %d entries, want 2", len(m.entries))
	}
	if m.usage.responses != 1 || m.usage.total.Total() != 60 {
		t.Fatalf("initial usage rebuild: %+v", m.usage)
	}

	m, cmd := submitText(t, m, "/branch "+firstID)
	if cmd != nil {
		t.Fatal("branch should be synchronous")
	}
	if got := len(h.Agent.Messages()); got != 1 {
		t.Fatalf("branch left %d messages, want 1", got)
	}
	if len(m.entries) != 2 || m.entries[0].text != "first" || m.entries[1].kind != entrySystem {
		t.Fatalf("branch transcript not rebuilt: %#v", m.entries)
	}
	if strings.Contains(m.entries[0].text, "later") {
		t.Fatal("abandoned branch message is still visible")
	}
	if m.usage.responses != 0 || m.usage.total.Total() != 0 {
		t.Fatalf("branch retained abandoned usage: %+v", m.usage)
	}

	m, cmd = submitText(t, m, "/resume")
	if cmd != nil {
		t.Fatal("resume should be synchronous")
	}
	if got := len(h.Agent.Messages()); got != 1 {
		t.Fatalf("resume restored %d messages, want 1", got)
	}
	if m.usage.responses != 0 {
		t.Fatalf("resume restored abandoned usage: %+v", m.usage)
	}
}

func TestResizeAndScrollFollowBehavior(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	for i := 0; i < 80; i++ {
		m.appendSystem(strings.Repeat("x", 20), false)
	}
	m.refreshTranscript(true)
	if !m.viewport.AtBottom() {
		t.Fatal("new content should follow at bottom")
	}
	m.viewport.GotoTop()
	m.appendSystem("new", false)
	m.refreshTranscript(m.viewport.AtBottom())
	if !m.viewport.AtTop() {
		t.Fatal("new content pulled user away from history")
	}

	for _, size := range []struct{ width, height int }{{40, 10}, {20, 5}, {1, 1}, {2, 2}, {80, 4}} {
		updated, _ := updateModel(t, m, tea.WindowSizeMsg{Width: size.width, Height: size.height})
		m = updated
		view := m.View()
		if got := lipgloss.Height(view); got != size.height {
			t.Errorf("view at %dx%d has height %d:\n%q", size.width, size.height, got, view)
		}
		if size.width >= 3 && lipgloss.Width(view) > size.width {
			t.Errorf("view at %dx%d exceeds width: %d", size.width, size.height, lipgloss.Width(view))
		}
	}
}

func TestThinkingTraceAutoAndManualFolding(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	thinking := "plan first\n" + strings.Repeat("reasoning ", 24) + "\nTHINKING_TAIL"
	message := agent.AgentMessage{Message: llm.Message{Role: llm.RoleAssistant}}
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventMessageStart, Message: &message})
	message.Content = []llm.Content{
		&llm.ThinkingContent{Thinking: thinking},
		&llm.TextContent{Text: "final answer"},
	}
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventMessageUpdate, Message: &message})
	if !m.entries[0].running {
		t.Fatal("streaming assistant entry should be active")
	}
	if view := m.View(); !strings.Contains(view, "Thinking [running]") || !strings.Contains(view, "THINKING_TAIL") {
		t.Fatalf("active thinking was not expanded:\n%s", view)
	}

	message.StopReason = llm.StopStop
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventMessageEnd, Message: &message})
	if m.entries[0].running {
		t.Fatal("completed assistant entry is still active")
	}
	view := m.View()
	if !strings.Contains(view, "Thinking [folded") || !strings.Contains(view, "plan first") || !strings.Contains(view, "final answer") {
		t.Fatalf("completed thinking summary missing:\n%s", view)
	}
	if strings.Contains(view, "THINKING_TAIL") {
		t.Fatalf("completed thinking was not automatically folded:\n%s", view)
	}

	updated, _ := updateModel(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m = updated
	if m.traceMode != traceModeExpanded || !strings.Contains(m.View(), "THINKING_TAIL") {
		t.Fatalf("ctrl+o did not expand trace: mode=%s\n%s", m.traceMode, m.View())
	}
	updated, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m = updated
	if m.traceMode != traceModeCollapsed || strings.Contains(m.View(), "THINKING_TAIL") {
		t.Fatalf("ctrl+o did not collapse trace: mode=%s\n%s", m.traceMode, m.View())
	}
}

func TestToolTraceAutoFolding(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	output := "first line\n" + strings.Repeat("working output ", 20) + "\nTOOL_TAIL"
	m = applyAgentEvent(t, m, agent.Event{
		Type: agent.EventToolExecutionStart, ToolCallID: "trace-call", ToolName: "bash", Args: json.RawMessage(`{"command":"long"}`),
	})
	m = applyAgentEvent(t, m, agent.Event{
		Type: agent.EventToolExecutionUpdate, ToolCallID: "trace-call", ToolName: "bash",
		PartialResult: &agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: output}}},
	})
	if view := m.View(); !strings.Contains(view, "[running]") || !strings.Contains(view, "TOOL_TAIL") {
		t.Fatalf("active tool output was not expanded:\n%s", view)
	}

	m = applyAgentEvent(t, m, agent.Event{
		Type: agent.EventToolExecutionEnd, ToolCallID: "trace-call", ToolName: "bash", IsError: true,
		Result: &agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: output}}},
	})
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventAgentEnd})
	view := m.View()
	for _, want := range []string{"trace-call", "[error]", "output [folded", "first line"} {
		if !strings.Contains(view, want) {
			t.Errorf("folded tool trace does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "TOOL_TAIL") {
		t.Fatalf("completed tool output was not automatically folded:\n%s", view)
	}
}

func TestTraceCommands(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	for _, test := range []struct {
		command string
		want    traceMode
	}{
		{"/trace expand", traceModeExpanded},
		{"/trace collapse", traceModeCollapsed},
		{"/trace auto", traceModeAuto},
	} {
		updated, cmd := submitText(t, m, test.command)
		m = updated
		if cmd != nil {
			t.Fatalf("%s returned unexpected async work", test.command)
		}
		if m.traceMode != test.want {
			t.Fatalf("%s set mode %s, want %s", test.command, m.traceMode, test.want)
		}
	}

	m, cmd := submitText(t, m, "/trace")
	if cmd != nil || !strings.Contains(m.entries[len(m.entries)-1].text, "当前运行轨迹模式：auto") {
		t.Fatalf("/trace did not report current mode: %#v", m.entries[len(m.entries)-1])
	}
	m, cmd = submitText(t, m, "/trace invalid")
	if cmd != nil || !m.entries[len(m.entries)-1].failed || m.traceMode != traceModeAuto {
		t.Fatalf("invalid /trace changed state incorrectly: mode=%s entry=%#v", m.traceMode, m.entries[len(m.entries)-1])
	}
}

func TestOutputPreviewIsRuneSafe(t *testing.T) {
	if got := truncateRunes("界面上🙂", 3); got != "界面…" {
		t.Fatalf("unexpected rune truncation: %q", got)
	}
	preview := outputPreview(strings.Repeat("界\n", 40))
	if !strings.Contains(preview, "省略") {
		t.Fatalf("long output was not summarized: %q", preview)
	}
}

func TestRuntimeCancelDoesNotLeakIntoNextPrompt(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	ctx, _ := m.runtime.begin()
	m.runtime.clearCurrent()
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("finished context has error %v, want context.Canceled", err)
	}
}
