package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valley3405/pi-go/pkg/agent"
	"github.com/valley3405/pi-go/pkg/llm"
)

const compactionPrompt = `Summarize the conversation so far for an AI coding agent that will continue working. Preserve:
- The user's original goal and requirements
- Decisions made and their rationale
- Files created/modified and key code details
- Errors encountered and how they were resolved
- Next steps

Be concise but complete. Write in the same language as the conversation.`

// Compact 用 LLM 对当前分支的旧消息生成摘要，append 一条 compaction entry
// （对应 pi 的 compaction 流程）。cutoff 之后的消息原样保留。
//
// keepRecent 指定保留最近几条消息不压缩。
func Compact(ctx context.Context, s *Session, streamFn llm.StreamFn, model llm.Model, opts llm.StreamOptions, keepRecent int) (string, error) {
	if keepRecent < 0 {
		return "", fmt.Errorf("keepRecent must be non-negative")
	}
	refs, err := s.messageRefs()
	if err != nil {
		return "", err
	}
	if len(refs) <= keepRecent {
		return "", fmt.Errorf("nothing to compact (%d messages, keepRecent=%d)", len(refs), keepRecent)
	}

	cut := len(refs) - keepRecent
	for cut < len(refs) && refs[cut].message.Role == llm.RoleToolResult {
		cut++
	}
	toSummarize := make([]agent.AgentMessage, cut)
	for i := range toSummarize {
		toSummarize[i] = refs[i].message
	}
	firstKeptEntryID := ""
	if cut < len(refs) {
		firstKeptEntryID = refs[cut].entryID
	}

	var b strings.Builder
	for _, m := range toSummarize {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Text())
		for _, tc := range m.ToolCalls() {
			fmt.Fprintf(&b, "  → tool %s(%s)\n", tc.Name, truncate(string(tc.Arguments), 200))
		}
		if m.Role == llm.RoleToolResult {
			// toolResult 的 Text() 已覆盖
		}
	}

	llmCtx := llm.Context{
		SystemPrompt: compactionPrompt,
		Messages:     []llm.Message{llm.NewUserMessage(b.String())},
	}
	stream := streamFn(ctx, model, llmCtx, opts)
	for range stream.Events() {
	}
	result, err := stream.Result()
	if err != nil {
		return "", err
	}
	summary := result.Text()
	if summary == "" {
		return "", fmt.Errorf("compaction produced empty summary")
	}

	// 技能正文随旧消息一起被摘要吞掉；把最近一次有效加载的技能正文
	// 按预算原样重注入摘要（保留区已有技能结果时跳过，避免重复）。
	kept := make([]agent.AgentMessage, 0, len(refs)-cut)
	for i := cut; i < len(refs); i++ {
		kept = append(kept, refs[i].message)
	}
	summary = reinjectSkillBody(summary, toSummarize, kept)

	_, err = s.AppendCustom(EntryCompaction, CompactionPayload{
		Summary:          summary,
		FirstKeptEntryID: firstKeptEntryID,
		TokensBefore:     estimateTokens(toSummarize),
	})
	return summary, err
}

// maxSkillReinjectRunes 是压缩时重注入技能正文的字符预算。
const maxSkillReinjectRunes = 8000

// reinjectSkillBody 在压缩摘要末尾附加上一次有效技能加载的完整正文。
// compacted 是被摘要替代的消息，kept 是原样保留的消息。
func reinjectSkillBody(summary string, compacted, kept []agent.AgentMessage) string {
	for _, m := range kept {
		if skillNameFromPayload(m.Payload) != "" {
			return summary // 保留区已有技能结果，模型仍能看到正文
		}
	}
	for i := len(compacted) - 1; i >= 0; i-- {
		m := compacted[i]
		if m.Role != llm.RoleToolResult || m.IsError || skillNameFromPayload(m.Payload) == "" {
			continue
		}
		body := m.Text()
		if body == "" {
			return summary
		}
		var note string
		if runes := []rune(body); len(runes) > maxSkillReinjectRunes {
			body = string(runes[:maxSkillReinjectRunes])
			note = fmt.Sprintf(" (truncated to %d chars)", maxSkillReinjectRunes)
		}
		return summary + "\n\n---\nThe most recently loaded skill content is preserved verbatim" + note + ":\n\n" + body
	}
	return summary
}

// skillNameFromPayload 从结果消息的 Payload 提取技能名；
// 兼容内存中的 SkillResultDetails 和 session JSONL 解码出的 map。
func skillNameFromPayload(payload any) string {
	if payload == nil {
		return ""
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	var details SkillResultDetails
	if err := json.Unmarshal(data, &details); err != nil {
		return ""
	}
	return details.SkillName
}

func estimateTokens(msgs []agent.AgentMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text()) / 4
		for _, tc := range m.ToolCalls() {
			n += len(tc.Arguments) / 4
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ShouldCompact 按 token 估算判断是否需要压缩。
func ShouldCompact(msgs []agent.AgentMessage, contextWindow int) bool {
	if contextWindow <= 0 {
		return false
	}
	return estimateTokens(msgs) > contextWindow*3/4
}
