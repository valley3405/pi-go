package tui

import (
	"fmt"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

type entryKind uint8

const (
	entryUser entryKind = iota
	entryAssistant
	entryTool
	entrySystem
)

type transcriptEntry struct {
	kind       entryKind
	text       string
	thinking   string
	name       string
	args       string
	output     string
	callID     string
	running    bool
	failed     bool
	stopReason llm.StopReason
}

func (m *Model) rebuildTranscript() {
	m.entries = nil
	m.toolEntries = make(map[string]int)
	m.currentEntry = -1
	for _, message := range m.harness.Agent.Messages() {
		if message.Custom {
			continue
		}
		switch message.Role {
		case llm.RoleUser, llm.RoleAssistant:
			entry := messageEntry(message.Message)
			if message.Role == llm.RoleUser || entry.text != "" || entry.thinking != "" {
				m.entries = append(m.entries, entry)
			}
		case llm.RoleToolResult:
			entry := messageEntry(message.Message)
			m.entries = append(m.entries, entry)
			m.toolEntries[toolKey(entry.callID, entry.name)] = len(m.entries) - 1
		}
	}
}

func messageEntry(message llm.Message) transcriptEntry {
	entry := transcriptEntry{stopReason: message.StopReason}
	switch message.Role {
	case llm.RoleUser:
		entry.kind = entryUser
		entry.text = message.Text()
	case llm.RoleAssistant:
		entry.kind = entryAssistant
		entry.text = message.Text()
		entry.thinking = thinkingText(message.Content)
		entry.failed = message.StopReason == llm.StopError || message.StopReason == llm.StopAborted
	case llm.RoleToolResult:
		entry.kind = entryTool
		entry.name = message.ToolName
		entry.callID = message.ToolCallID
		entry.output = contentText(message.Content)
		entry.failed = message.IsError
	}
	return entry
}

func contentText(content []llm.Content) string {
	var b strings.Builder
	for _, block := range content {
		switch block := block.(type) {
		case *llm.TextContent:
			b.WriteString(block.Text)
		case *llm.ImageContent:
			b.WriteString("[image ")
			b.WriteString(block.MimeType)
			b.WriteByte(']')
		}
	}
	return b.String()
}

func thinkingText(content []llm.Content) string {
	var b strings.Builder
	for _, block := range content {
		if thinking, ok := block.(*llm.ThinkingContent); ok {
			b.WriteString(thinking.Thinking)
		}
	}
	return b.String()
}

func (m *Model) applyAgentEvent(event agent.Event) {
	switch event.Type {
	case agent.EventAgentStart:
		m.busy = true
		m.activity = "正在等待模型响应…"
		m.notice = ""
	case agent.EventAgentEnd:
		m.busy = false
		m.activity = ""
		m.notice = ""
		for i := range m.entries {
			m.entries[i].running = false
		}
		m.runtime.clearCurrent()
	case agent.EventTurnStart:
		if m.activity == "" {
			m.activity = "正在生成…"
		}
	case agent.EventMessageStart, agent.EventMessageUpdate, agent.EventMessageEnd:
		m.applyMessageEvent(event)
	case agent.EventToolExecutionStart:
		m.busy = true
		m.activity = toolActivity(event.ToolName)
		entry := transcriptEntry{
			kind:    entryTool,
			name:    event.ToolName,
			args:    string(event.Args),
			callID:  event.ToolCallID,
			running: true,
		}
		m.entries = append(m.entries, entry)
		m.toolEntries[toolKey(event.ToolCallID, event.ToolName)] = len(m.entries) - 1
	case agent.EventToolExecutionUpdate:
		m.activity = toolActivity(event.ToolName)
		if index, ok := m.toolIndex(event.ToolCallID, event.ToolName); ok {
			if event.PartialResult != nil {
				m.entries[index].output = contentText(event.PartialResult.Content)
			}
			if len(event.Args) > 0 {
				m.entries[index].args = string(event.Args)
			}
		}
	case agent.EventToolExecutionEnd:
		m.activity = toolActivity(event.ToolName)
		index, ok := m.toolIndex(event.ToolCallID, event.ToolName)
		if !ok {
			m.entries = append(m.entries, transcriptEntry{kind: entryTool, name: event.ToolName, callID: event.ToolCallID})
			index = len(m.entries) - 1
			m.toolEntries[toolKey(event.ToolCallID, event.ToolName)] = index
		}
		entry := &m.entries[index]
		entry.running = false
		entry.failed = event.IsError
		if event.Result != nil {
			entry.output = contentText(event.Result.Content)
		}
	}
}

func (m *Model) applyMessageEvent(event agent.Event) {
	if event.Message == nil || event.Message.Custom {
		return
	}
	role := event.Message.Role
	if role != llm.RoleUser && role != llm.RoleAssistant && role != llm.RoleToolResult {
		return
	}

	if event.Type == agent.EventMessageStart {
		m.currentEntry = -1
		if role == llm.RoleToolResult {
			return
		}
		if role == llm.RoleAssistant {
			m.activity = "正在生成…"
		}
		entry := messageEntry(event.Message.Message)
		entry.running = role == llm.RoleAssistant
		m.entries = append(m.entries, entry)
		m.currentEntry = len(m.entries) - 1
		return
	}
	if role == llm.RoleToolResult {
		return
	}

	entry := messageEntry(event.Message.Message)
	entry.running = role == llm.RoleAssistant && event.Type != agent.EventMessageEnd
	index := m.currentEntry
	if index < 0 || index >= len(m.entries) {
		m.entries = append(m.entries, entry)
		index = len(m.entries) - 1
		m.currentEntry = index
	} else {
		m.entries[index] = entry
	}

	if event.Type == agent.EventMessageEnd {
		entry = m.entries[index]
		if role == llm.RoleAssistant && entry.text == "" && entry.thinking == "" && !entry.failed {
			m.entries = append(m.entries[:index], m.entries[index+1:]...)
			m.reindexTools(index)
		}
		m.currentEntry = -1
	}
}

func (m *Model) reindexTools(removed int) {
	for key, index := range m.toolEntries {
		switch {
		case index == removed:
			delete(m.toolEntries, key)
		case index > removed:
			m.toolEntries[key] = index - 1
		}
	}
}

func (m *Model) toolIndex(callID, name string) (int, bool) {
	index, ok := m.toolEntries[toolKey(callID, name)]
	return index, ok && index >= 0 && index < len(m.entries)
}

func toolKey(callID, name string) string {
	if callID != "" {
		return callID
	}
	return "name:" + name
}

func toolActivity(name string) string {
	if name == "" {
		return "正在执行工具…"
	}
	return fmt.Sprintf("正在执行 %s…", name)
}

func (m *Model) appendSystem(text string, failed bool) {
	m.entries = append(m.entries, transcriptEntry{kind: entrySystem, text: text, failed: failed})
}
