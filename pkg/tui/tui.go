// Package tui provides the interactive Bubble Tea client for goagent.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/tools"
)

const (
	defaultWidth  = 80
	defaultHeight = 24
	maxInputRows  = 5
)

type runtimeState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (r *runtimeState) begin() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel = cancel
	r.mu.Unlock()
	return ctx, cancel
}

func (r *runtimeState) cancelCurrent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *runtimeState) clear(cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel = nil
	cancel()
}

func (r *runtimeState) clearCurrent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

type agentEventMsg struct {
	event agent.Event
}

type promptDoneMsg struct {
	err error
}

type compactDoneMsg struct {
	summary string
	err     error
}

type bashDoneMsg struct {
	output string
	err    error
	failed bool
}

type traceMode uint8

const (
	traceModeAuto traceMode = iota
	traceModeExpanded
	traceModeCollapsed
)

func (m traceMode) String() string {
	switch m {
	case traceModeExpanded:
		return "expanded"
	case traceModeCollapsed:
		return "collapsed"
	default:
		return "auto"
	}
}

// Model is the Bubble Tea model for an agent session.
type Model struct {
	harness *harness.Harness
	runtime *runtimeState

	viewport viewport.Model
	input    textarea.Model
	entries  []transcriptEntry

	toolEntries  map[string]int
	currentEntry int
	width        int
	height       int
	busy         bool
	activity     string
	notice       string
	traceMode    traceMode
	usage        usageStats
}

// New creates a TUI model backed by h.
func New(h *harness.Harness) Model {
	input := textarea.New()
	input.Prompt = "❯ "
	input.Placeholder = "输入消息，或输入 /help 查看命令"
	input.ShowLineNumbers = false
	input.MaxHeight = maxInputRows
	input.MaxWidth = 0
	input.SetHeight(1)
	input.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "alt+enter", "ctrl+j"),
		key.WithHelp("shift+enter/alt+enter", "newline"),
	)
	input.Focus()

	m := Model{
		harness:      h,
		runtime:      &runtimeState{},
		viewport:     viewport.New(defaultWidth, 1),
		input:        input,
		toolEntries:  make(map[string]int),
		currentEntry: -1,
		width:        defaultWidth,
		height:       defaultHeight,
	}
	m.rebuildTranscript()
	m.resize(defaultWidth, defaultHeight)
	m.refreshTranscript(true)
	return m
}

// Run starts the full-screen TUI until the user quits.
func Run(h *harness.Harness) error {
	if h == nil || h.Agent == nil {
		return errors.New("tui requires an agent harness")
	}
	m := New(h)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	unsubscribe := h.Agent.Subscribe(func(ev agent.Event) {
		p.Send(agentEventMsg{event: ev})
	})
	defer func() {
		unsubscribe()
		m.runtime.cancelCurrent()
		h.Agent.Abort()
		h.Agent.WaitForIdle()
	}()
	_, err := p.Run()
	return err
}

// Init starts the input cursor.
func (m Model) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles terminal, agent, and asynchronous command messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		follow := m.viewport.AtBottom()
		m.resize(msg.Width, msg.Height)
		m.refreshTranscript(follow)
		return m, nil

	case agentEventMsg:
		follow := m.viewport.AtBottom()
		m.applyAgentEvent(msg.event)
		m.refreshTranscript(follow)
		return m, nil

	case promptDoneMsg:
		if msg.err != nil {
			m.busy = false
			m.activity = ""
			m.notice = ""
			m.appendSystem("prompt error: "+msg.err.Error(), true)
			m.refreshTranscript(true)
		}
		return m, nil

	case compactDoneMsg:
		m.busy = false
		m.activity = ""
		m.notice = ""
		if msg.err != nil {
			m.appendSystem("compact error: "+msg.err.Error(), true)
		} else {
			m.rebuildTranscript()
			m.appendSystem(fmt.Sprintf("上下文已压缩（摘要 %d 字符）", len([]rune(msg.summary))), false)
		}
		m.refreshTranscript(true)
		return m, nil

	case bashDoneMsg:
		m.busy = false
		m.activity = ""
		m.notice = ""
		output := msg.output
		if msg.err != nil {
			if output != "" {
				output += "\n"
			}
			output += msg.err.Error()
		}
		m.entries = append(m.entries, transcriptEntry{
			kind:   entryTool,
			name:   "bash",
			output: output,
			failed: msg.failed,
		})
		m.refreshTranscript(true)
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.busy {
				m.abortCurrent()
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.busy {
				m.abortCurrent()
				return m, nil
			}
			if m.input.Value() != "" {
				m.input.Reset()
				m.resize(m.width, m.height)
			}
			return m, nil
		case "ctrl+o":
			follow := m.viewport.AtBottom()
			if m.traceMode == traceModeExpanded || (m.traceMode == traceModeAuto && m.busy) {
				m.traceMode = traceModeCollapsed
			} else {
				m.traceMode = traceModeExpanded
			}
			m.refreshTranscript(follow)
			return m, nil
		case "enter":
			return m.submit()
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	}

	follow := m.viewport.AtBottom()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.resize(m.width, m.height)
	if follow {
		m.viewport.GotoBottom()
	}
	return m, cmd
}

func (m *Model) abortCurrent() {
	m.runtime.cancelCurrent()
	m.harness.Agent.Abort()
	m.notice = "正在中止当前任务…"
}

func (m Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	m.notice = ""
	if text == "" {
		return m, nil
	}
	if m.busy {
		m.notice = "当前任务仍在运行，按 Esc 或 Ctrl+C 中止"
		return m, nil
	}
	if text == "/quit" || text == "/exit" {
		return m, tea.Quit
	}

	m.input.Reset()
	m.resize(m.width, m.height)
	if strings.HasPrefix(text, "/") {
		return m.executeSlashCommand(text)
	}
	if strings.HasPrefix(text, "!") {
		return m.executeUserBash(strings.TrimSpace(strings.TrimPrefix(text, "!")))
	}

	m.busy = true
	m.activity = "正在等待模型响应…"
	ctx, done := m.runtime.begin()
	h := m.harness
	return m, func() tea.Msg {
		err := h.Prompt(ctx, text)
		if err != nil {
			m.runtime.clear(done)
			return promptDoneMsg{err: err}
		}
		return promptDoneMsg{}
	}
}

func (m Model) executeSlashCommand(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	command := fields[0]
	switch command {
	case "/help":
		m.appendSystem(helpText, false)
	case "/skills":
		m.appendSystem(m.skillsText(), false)
	case "/tree":
		m.appendSystem(m.treeText(), false)
	case "/usage":
		m.appendSystem(m.usage.detail(), false)
	case "/branch":
		if len(fields) != 2 {
			m.appendSystem("用法：/branch <entry-id>", true)
			break
		}
		if err := m.harness.Branch(fields[1]); err != nil {
			m.appendSystem("branch error: "+err.Error(), true)
		} else {
			m.rebuildTranscript()
			m.appendSystem("已切换到 entry "+fields[1], false)
		}
	case "/resume":
		if err := m.harness.Resume(); err != nil {
			m.appendSystem("resume error: "+err.Error(), true)
		} else {
			m.rebuildTranscript()
			m.appendSystem(fmt.Sprintf("会话已恢复，共 %d 条消息", len(m.harness.Agent.Messages())), false)
		}
	case "/trace":
		if len(fields) == 1 {
			m.appendSystem("当前运行轨迹模式："+m.traceMode.String()+"\n用法：/trace auto|expand|collapse", false)
			break
		}
		if len(fields) != 2 {
			m.appendSystem("用法：/trace auto|expand|collapse", true)
			break
		}
		valid := true
		switch fields[1] {
		case "auto":
			m.traceMode = traceModeAuto
		case "expand":
			m.traceMode = traceModeExpanded
		case "collapse":
			m.traceMode = traceModeCollapsed
		default:
			m.appendSystem("未知轨迹模式 "+fields[1]+"，用法：/trace auto|expand|collapse", true)
			valid = false
		}
		if valid {
			m.appendSystem("运行轨迹模式："+m.traceMode.String(), false)
		}
	case "/compact":
		m.busy = true
		m.activity = "正在压缩上下文…"
		ctx, done := m.runtime.begin()
		h := m.harness
		return m, func() tea.Msg {
			summary, err := h.Compact(ctx)
			if err == nil {
				err = h.Resume()
			}
			m.runtime.clear(done)
			return compactDoneMsg{summary: summary, err: err}
		}
	default:
		m.appendSystem("未知命令 "+command+"，输入 /help 查看可用命令", true)
	}
	m.refreshTranscript(true)
	return m, nil
}

func (m Model) executeUserBash(command string) (tea.Model, tea.Cmd) {
	if command == "" {
		m.appendSystem("用法：!<command>", true)
		m.refreshTranscript(true)
		return m, nil
	}
	m.entries = append(m.entries, transcriptEntry{kind: entryUser, text: "!" + command})
	m.busy = true
	m.activity = "正在执行用户命令…"
	m.refreshTranscript(true)
	ctx, done := m.runtime.begin()
	return m, func() tea.Msg {
		defer m.runtime.clear(done)
		args, _ := json.Marshal(map[string]any{"command": command})
		result, err := tools.Bash().Execute(ctx, "user-bash", args, nil)
		failed := err != nil || ctx.Err() != nil
		if details, ok := result.Details.(tools.BashResultDetails); ok && !details.Success {
			failed = true
		}
		return bashDoneMsg{output: contentText(result.Content), err: err, failed: failed}
	}
}

func (m Model) skillsText() string {
	if len(m.harness.Skills) == 0 {
		return "未加载 skills"
	}
	var b strings.Builder
	for i, skill := range m.harness.Skills {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s [%s] %s\n  %s", skill.Name, skill.Source, skill.Description, skill.Dir)
	}
	return b.String()
}

func (m Model) treeText() string {
	if m.harness.Session == nil {
		return "未启用 session 持久化"
	}
	entries := m.harness.Session.Entries()
	if len(entries) == 0 {
		return "当前分支暂无 entries"
	}
	leafID := m.harness.Session.LeafID()
	var b strings.Builder
	for i, entry := range entries {
		mark := "  "
		if entry.ID == leafID {
			mark = "* "
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s%s %s (parent=%s)", mark, entry.ID, entry.Type, entry.ParentID)
	}
	return b.String()
}

const helpText = `可用命令：
  /help             显示帮助
  /skills           查看已加载 skills
  /tree             查看当前 session 分支
  /usage            查看最近及累计 token 用量
  /branch <id>      从历史 entry 创建分支
  /compact          压缩当前上下文
  /trace <mode>     运行轨迹：auto / expand / collapse（Ctrl+O 手动切换）
  /resume           重新加载当前 session
  /quit, /exit      退出
  !<command>        直接执行 bash 命令`
