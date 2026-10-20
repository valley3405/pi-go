package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "236", Dark: "252"}).
			Background(lipgloss.AdaptiveColor{Light: "254", Dark: "237"})
	userLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "25", Dark: "39"})
	agentLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "88", Dark: "212"})
	toolLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "22", Dark: "114"})
	systemLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "245"})
	thinkingStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "244"})
	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "245"})
	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})
	toolOutputStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "238", Dark: "250"})
	approvalSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Reverse(true)
)

// View renders the complete terminal interface.
func (m Model) View() string {
	parts := make([]string, 0, 5)
	if m.height >= 3 {
		parts = append(parts, m.headerView())
	}
	if m.viewport.Height > 0 {
		parts = append(parts, m.viewportView())
	}
	if len(m.approvals) > 0 {
		parts = append(parts, m.approvalPanelView())
	}
	parts = append(parts, m.input.View())
	if m.height >= 4 {
		parts = append(parts, m.statusView())
	}
	if m.height >= 5 {
		parts = append(parts, m.helpView())
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m Model) headerView() string {
	model := m.harness.Agent.Snapshot().Model
	name := model.ID
	if name == "" {
		name = "unknown"
	}
	if model.Provider != "" {
		name = model.Provider + "/" + name
	}
	return headerStyle.MaxWidth(m.width).Render(" goagent · " + name)
}

func (m Model) statusView() string {
	status := "ready"
	switch {
	case m.notice != "":
		status = m.notice
	case m.busy && m.activity != "":
		status = "● " + m.activity
	case m.busy:
		status = "● working"
	}
	if usage := m.usage.status(); usage != "" {
		status += " · " + usage
	}
	if !m.busy && m.notice == "" {
		if m.harness.Session != nil {
			status += " · session " + m.harness.Session.ID()
		} else {
			status += " · no session"
		}
	}
	status += " · trace:" + m.traceMode.String()
	if m.policy != nil {
		status += " · perm:" + string(m.policy.CurrentMode())
	}
	style := mutedStyle
	if m.notice != "" && !m.busy {
		style = errorStyle
	}
	return style.MaxWidth(m.width).Render(status)
}

func (m Model) helpView() string {
	help := "Enter 发送 · Alt+Enter 换行 · Ctrl+O 轨迹 · PgUp/PgDn 滚动 · 鼠标拖拽复制 · Esc/Ctrl+C 中止/退出"
	if m.policy != nil {
		help = "Shift+Tab 权限 · " + help
	}
	if len(m.approvals) > 0 {
		help = "↑/↓/Tab 选择 · 1 允许 · 2 拒绝 · Enter 确认 · Ctrl+O 折叠/展开参数 · PgUp/PgDn 滚动参数 · Esc 中止"
	}
	return mutedStyle.MaxWidth(m.width).Render(help)
}

// approvalPanelView renders the pending approval with its full arguments and
// an explicit allow/deny selection.
func (m Model) approvalPanelView() string {
	req := m.approvals[0].request
	title := errorStyle.Bold(true).Render(fmt.Sprintf("审批 1/%d · %s", len(m.approvals), req.ToolName))
	var body string
	if m.approvalExpanded {
		body = m.approvalScroll.View()
	} else {
		args := string(req.Args)
		summary := fmt.Sprintf("参数 [已折叠 · %d chars · Ctrl+O 展开] %s", len([]rune(args)), singleLine(args, max(10, m.width-30)))
		body = mutedStyle.MaxWidth(m.width).Render(summary)
	}
	allow, deny := "  允许", "  拒绝"
	if m.approvalChoice == approvalChoiceAllow {
		allow = approvalSelectedStyle.Render("▸ 允许")
	} else {
		deny = approvalSelectedStyle.Render("▸ 拒绝")
	}
	options := lipgloss.JoinHorizontal(lipgloss.Top, allow, "    ", deny)
	return lipgloss.JoinVertical(lipgloss.Left, title, body, options)
}

// approvalBody returns the complete, untruncated approval content: the
// approval reason (if any) followed by the full pretty-printed arguments.
func (m Model) approvalBody() string {
	req := m.approvals[0].request
	var b strings.Builder
	if req.ToolDecision.RequireApproval && req.ToolDecision.Reason != "" {
		b.WriteString(req.ToolDecision.Reason)
		b.WriteString("\n\n")
	}
	b.WriteString(prettyJSON(string(req.Args)))
	return b.String()
}

func (m Model) approvalPanelHeight() int {
	if len(m.approvals) == 0 {
		return 0
	}
	if !m.approvalExpanded {
		return 3
	}
	body := lipgloss.NewStyle().Width(max(1, m.width)).Render(m.approvalBody())
	return clamp(lipgloss.Height(body), 1, max(3, m.height/3)) + 2
}

func (m *Model) resize(width, height int) {
	m.width = max(1, width)
	m.height = max(1, height)
	if m.width < 3 {
		m.input.Prompt = ""
	} else {
		m.input.Prompt = "❯ "
	}
	m.input.SetWidth(m.width)

	chrome := 0
	if m.height >= 3 {
		chrome++
	}
	if m.height >= 4 {
		chrome++
	}
	if m.height >= 5 {
		chrome++
	}
	available := max(1, m.height-chrome)
	maxInputHeight := available
	if available >= 2 {
		maxInputHeight--
	}
	m.input.SetHeight(clamp(m.input.LineCount(), 1, min(maxInputRows, max(1, maxInputHeight))))
	m.viewport.Width = m.width
	m.viewport.Height = max(0, available-m.input.Height()-m.approvalPanelHeight())
	m.syncApprovalScroll()
}

func (m *Model) refreshTranscript(follow bool) {
	content := m.renderTranscript(m.viewport.Width)
	m.viewport.SetContent(content)
	m.plainLines = strings.Split(stripANSI(content), "\n")
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m Model) renderTranscript(width int) string {
	if len(m.entries) == 0 {
		welcome := "开始一段 coding session。\n\n输入消息与 agent 对话，输入 /help 查看会话命令。\n历史记录可用 PgUp/PgDn 或鼠标滚轮浏览。"
		return mutedStyle.Width(max(1, width)).Render(welcome)
	}
	var b strings.Builder
	for i, entry := range m.entries {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderEntry(entry, width, m.traceExpanded(entry)))
	}
	return b.String()
}

func (m Model) traceExpanded(entry transcriptEntry) bool {
	switch m.traceMode {
	case traceModeExpanded:
		return true
	case traceModeCollapsed:
		return false
	default:
		return entry.running
	}
}

func renderEntry(entry transcriptEntry, width int, expandTrace bool) string {
	width = max(1, width)
	switch entry.kind {
	case entryUser:
		return renderMessage(userLabelStyle.Render("You"), entry.text, nil, width)
	case entryAssistant:
		body := entry.text
		if entry.failed {
			terminal := "agent run ended: " + string(entry.stopReason)
			if body == "" {
				body = terminal
			} else {
				body += "\n\n" + terminal
			}
		}
		if body == "" {
			body = "…"
		}
		var bodyStyle *lipgloss.Style
		if entry.failed {
			bodyStyle = &errorStyle
		}
		var prefix string
		if entry.thinking != "" {
			var trace string
			if expandTrace {
				label := "Thinking"
				if entry.running {
					label += " [running]"
				}
				trace = label + "\n" + entry.thinking
			} else {
				trace = fmt.Sprintf("Thinking [folded · %d chars]\n%s", len([]rune(entry.thinking)), singleLine(entry.thinking, 160))
			}
			prefix = thinkingStyle.Render(trace) + "\n\n"
		}
		return renderMessage(agentLabelStyle.Render("Agent"), prefix+body, bodyStyle, width)
	case entryTool:
		status := "[ok]"
		labelStyle := toolLabelStyle
		if entry.running {
			status = "[running]"
		} else if entry.failed {
			status = "[error]"
			labelStyle = errorStyle
		}
		label := labelStyle.Render(fmt.Sprintf("%s %s", entry.name, status))
		if entry.callID != "" {
			label += mutedStyle.Render(" · " + entry.callID)
		}
		var b strings.Builder
		b.WriteString(label)
		if entry.args != "" {
			b.WriteByte('\n')
			if expandTrace {
				b.WriteString(mutedStyle.Width(width).MaxWidth(width).Render("args: " + strings.TrimSpace(entry.args)))
			} else {
				b.WriteString(mutedStyle.MaxWidth(width).Render("args: " + singleLine(entry.args, 160)))
			}
		}
		if entry.output != "" {
			b.WriteByte('\n')
			if expandTrace {
				b.WriteString(toolOutputStyle.Width(width).MaxWidth(width).Render(outputPreview(entry.output)))
			} else {
				lines := len(strings.Split(entry.output, "\n"))
				summary := fmt.Sprintf("output [folded · %d lines]: %s", lines, singleLine(entry.output, 160))
				b.WriteString(toolOutputStyle.MaxWidth(width).Render(summary))
			}
		}
		return b.String()
	case entrySystem:
		style := mutedStyle
		if entry.failed {
			style = errorStyle
		}
		return renderMessage(style.Render("System"), entry.text, &style, width)
	default:
		return ""
	}
}

func renderMessage(label, body string, style *lipgloss.Style, width int) string {
	bodyStyle := lipgloss.NewStyle()
	if style != nil {
		bodyStyle = *style
	}
	return label + "\n" + bodyStyle.Width(width).MaxWidth(width).Render(body)
}

func singleLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	return truncateRunes(text, limit)
}

func outputPreview(text string) string {
	const (
		maxLines = 24
		maxRunes = 6000
	)
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		head := maxLines * 2 / 3
		tail := maxLines - head
		omitted := len(lines) - head - tail
		lines = append(lines[:head], append([]string{fmt.Sprintf("… 省略 %d 行 …", omitted)}, lines[len(lines)-tail:]...)...)
	}
	text = strings.Join(lines, "\n")
	runes := []rune(text)
	if len(runes) > maxRunes {
		head := maxRunes * 2 / 3
		tail := maxRunes - head
		text = string(runes[:head]) + "\n… 输出已截断 …\n" + string(runes[len(runes)-tail:])
	}
	return text
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// prettyJSON formats raw JSON arguments for display, preserving the complete
// content; non-JSON input is returned as-is.
func prettyJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "(无参数)"
	}
	var buf bytes.Buffer
	if json.Indent(&buf, []byte(trimmed), "", "  ") == nil {
		return buf.String()
	}
	return trimmed
}

func clamp(value, low, high int) int {
	return min(high, max(low, value))
}
