package harness

import (
	"context"
	"fmt"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
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

	_, err = s.AppendCustom(EntryCompaction, CompactionPayload{
		Summary:          summary,
		FirstKeptEntryID: firstKeptEntryID,
		TokensBefore:     estimateTokens(toSummarize),
	})
	return summary, err
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
