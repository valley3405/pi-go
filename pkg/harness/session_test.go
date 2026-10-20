package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/llm"
)

func TestSessionTreeAndBranch(t *testing.T) {
	dir := t.TempDir()
	s, err := harness.CreateSession(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}

	// 构建线性历史：a1 → a2 → a3
	id1, _ := s.AppendMessage(agent.NewUserMessage("one"))
	id2, _ := s.AppendMessage(agent.NewUserMessage("two"))
	_, _ = s.AppendMessage(agent.NewUserMessage("three"))

	msgs, _ := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// 分叉回 id1，追加新消息
	if err := s.Branch(id1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(agent.NewUserMessage("alt-two")); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.Messages()
	if len(msgs) != 2 || msgs[1].Text() != "alt-two" {
		t.Fatalf("branch context wrong: %v", msgs)
	}
	s.Close()

	// 重开：leaf 应在新分支末端
	s2, err := harness.OpenSession(filepath.Join(dir, s.ID()+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	msgs2, _ := s2.Messages()
	if len(msgs2) != 2 || msgs2[1].Text() != "alt-two" {
		t.Fatalf("reopened session wrong: %v", msgs2)
	}

	// 分叉回 id2（旧分支仍在树中）
	if err := s2.Branch(id2); err != nil {
		t.Fatal(err)
	}
	msgs3, _ := s2.Messages()
	if len(msgs3) != 2 || msgs3[1].Text() != "two" {
		t.Fatalf("rebranch wrong: %v", msgs3)
	}
}

func TestCompactionEntryRebuild(t *testing.T) {
	dir := t.TempDir()
	s, err := harness.CreateSession(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.AppendMessage(agent.NewUserMessage("old message"))
	keptID, _ := s.AppendMessage(agent.NewUserMessage("kept message"))
	_, _ = s.AppendCustom(harness.EntryCompaction, harness.CompactionPayload{
		Summary: "summary of old stuff", FirstKeptEntryID: keptID, TokensBefore: 100,
	})
	_, _ = s.AppendMessage(agent.NewUserMessage("recent"))

	msgs, err := s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected summary + 2 kept messages, got %d", len(msgs))
	}
	if got := msgs[0].Text(); got != "[Context compaction] Earlier conversation was summarized:\n\nsummary of old stuff" {
		t.Fatalf("compaction summary missing or misplaced: %q", got)
	}
	if got := msgs[1].Text(); got != "kept message" {
		t.Fatalf("expected kept message, got %q", got)
	}
	if got := msgs[2].Text(); got != "recent" {
		t.Fatalf("expected recent message, got %q", got)
	}
}

func TestCompactTruncatesContextAcrossCompactionAndResume(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxAssistant("first summary"),
		llm.FauxAssistant("second summary"),
	}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	s, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var entryIDs []string
	for _, text := range []string{"one", "two", "three", "four", "five", "six"} {
		id, err := s.AppendMessage(agent.NewUserMessage(text))
		if err != nil {
			t.Fatal(err)
		}
		entryIDs = append(entryIDs, id)
	}

	assertCompacted := func(msgs []agent.AgentMessage, summary string) {
		t.Helper()
		if len(msgs) != 3 {
			t.Fatalf("expected summary + 2 kept messages, got %d", len(msgs))
		}
		if got := msgs[0].Text(); got != "[Context compaction] Earlier conversation was summarized:\n\n"+summary {
			t.Fatalf("unexpected compaction summary: %q", got)
		}
		if got := msgs[1].Text(); got != "five" {
			t.Fatalf("expected first kept message %q, got %q", "five", got)
		}
		if got := msgs[2].Text(); got != "six" {
			t.Fatalf("expected second kept message %q, got %q", "six", got)
		}
	}

	if _, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 2); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	assertCompacted(msgs, "first summary")

	if _, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 2); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	assertCompacted(msgs, "second summary")
	lastCompactionID := s.LeafID()

	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := harness.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	msgs, err = reopened.Messages()
	if err != nil {
		t.Fatal(err)
	}
	assertCompacted(msgs, "second summary")

	if err := reopened.Branch(entryIDs[1]); err != nil {
		t.Fatal(err)
	}
	msgs, err = reopened.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Text() != "one" || msgs[1].Text() != "two" {
		t.Fatalf("branch before compaction should retain raw history: %v", msgs)
	}

	if err := reopened.Branch(lastCompactionID); err != nil {
		t.Fatal(err)
	}
	msgs, err = reopened.Messages()
	if err != nil {
		t.Fatal(err)
	}
	assertCompacted(msgs, "second summary")
}

func TestCompactDoesNotKeepOrphanToolResult(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("summary")}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	s, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.AppendMessage(agent.NewUserMessage("read a file"))
	_, _ = s.AppendMessage(agent.AgentMessage{Message: llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.Content{&llm.ToolCallContent{
			ID: "call-1", Name: "read", Arguments: []byte(`{"path":"a.txt"}`),
		}},
	}})
	_, _ = s.AppendMessage(agent.AgentMessage{Message: llm.NewToolResultMessage(
		"call-1", "read", []llm.Content{&llm.TextContent{Text: "file contents"}}, false,
	)})
	_, _ = s.AppendMessage(agent.NewUserMessage("next request"))

	if _, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 2); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected summary + latest user message, got %d messages", len(msgs))
	}
	for _, msg := range msgs {
		if msg.Role == llm.RoleToolResult {
			t.Fatalf("kept context contains an orphan tool result: %v", msgs)
		}
	}
	if got := msgs[1].Text(); got != "next request" {
		t.Fatalf("expected latest user message, got %q", got)
	}
}

func TestHarnessPromptPersistence(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("answer")}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	a := agent.New(agent.Options{SystemPrompt: "t", Model: model, StreamFn: reg.StreamSimple})
	dir := t.TempDir()
	s, err := harness.CreateSession(dir, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := harness.New(a, s, reg.StreamSimple, llm.StreamOptions{})

	if err := h.Prompt(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()

	// 会话文件应有 user + assistant 两条 message entry
	count := 0
	for _, e := range s.Entries() {
		if e.Type == harness.EntryMessage {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 persisted message entries, got %d", count)
	}
}

func TestBranchWithSummary(t *testing.T) {
	dir := t.TempDir()
	s, _ := harness.CreateSession(dir, "/tmp")
	defer s.Close()
	id1, _ := s.AppendMessage(agent.NewUserMessage("main path"))
	_, _ = s.AppendMessage(agent.NewUserMessage("doomed"))

	if err := s.BranchWithSummary(id1, "the doomed path tried X"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages()
	// 新路径：main path + branch_summary（作为 user 消息注入）
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if got := msgs[1].Text(); len(got) < 7 || got[:7] != "[Branch" {
		t.Fatalf("branch summary missing: %q", got)
	}
}

func skillResultMessage(callID, name, body string, isErr bool) agent.AgentMessage {
	return agent.AgentMessage{
		Message: llm.NewToolResultMessage(callID, "skill", []llm.Content{&llm.TextContent{Text: body}}, isErr),
		Payload: harness.SkillResultDetails{SkillName: name, Dir: "/skills/" + name},
	}
}

func TestCompactReinjectsLatestSkillBody(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("summary")}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	s, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.AppendMessage(agent.NewUserMessage("task"))
	_, _ = s.AppendMessage(skillResultMessage("c1", "commit", "# Skill: commit\n\nOLD skill body", false))
	_, _ = s.AppendMessage(agent.NewUserMessage("more work"))
	_, _ = s.AppendMessage(skillResultMessage("c2", "review", "# Skill: review\n\nLATEST skill body", false))
	_, _ = s.AppendMessage(agent.NewUserMessage("latest"))

	summary, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 只重注入最近一次有效调用的正文
	if !strings.Contains(summary, "LATEST skill body") {
		t.Fatalf("latest skill body not reinjected:\n%s", summary)
	}
	if strings.Contains(summary, "OLD skill body") {
		t.Fatalf("older skill body should not be reinjected:\n%s", summary)
	}
	// 重建后的上下文同样可见
	msgs, err := s.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msgs[0].Text(), "LATEST skill body") {
		t.Fatalf("rebuilt context lost skill body:\n%s", msgs[0].Text())
	}
}

func TestCompactSkipsReinjectWhenSkillInKeptRegion(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("summary")}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	s, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.AppendMessage(agent.NewUserMessage("task"))
	_, _ = s.AppendMessage(agent.AgentMessage{Message: llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.Content{&llm.ToolCallContent{
			ID: "c1", Name: "skill", Arguments: []byte(`{"name":"commit"}`),
		}},
	}})
	_, _ = s.AppendMessage(skillResultMessage("c1", "commit", "# Skill: commit\n\nbody", false))
	_, _ = s.AppendMessage(agent.NewUserMessage("latest"))

	// 技能结果在保留区（assistant  toolcall 开头，结果不落在边界上）：摘要不应附带重注入段落
	summary, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "preserved verbatim") {
		t.Fatalf("skill already in kept region should not be reinjected:\n%s", summary)
	}
}

func TestCompactSkipsFailedSkillResultAndTruncatesBudget(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("summary"), llm.FauxAssistant("summary2")}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	s, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 错误结果不重注入
	_, _ = s.AppendMessage(agent.NewUserMessage("task"))
	_, _ = s.AppendMessage(skillResultMessage("c1", "commit", "unknown skill", true))
	_, _ = s.AppendMessage(agent.NewUserMessage("latest"))
	summary, err := harness.Compact(context.Background(), s, reg.StreamSimple, model, llm.StreamOptions{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "preserved verbatim") {
		t.Fatalf("failed skill result should not be reinjected:\n%s", summary)
	}

	// 超长正文按预算截断
	s2, err := harness.CreateSession(t.TempDir(), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	longBody := "# Skill: big\n\n" + strings.Repeat("界", 9000)
	_, _ = s2.AppendMessage(agent.NewUserMessage("task"))
	_, _ = s2.AppendMessage(skillResultMessage("c2", "big", longBody, false))
	_, _ = s2.AppendMessage(agent.NewUserMessage("latest"))
	summary, err = harness.Compact(context.Background(), s2, reg.StreamSimple, model, llm.StreamOptions{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "truncated") || strings.Contains(summary, strings.Repeat("界", 9000)) {
		t.Fatalf("long skill body should be truncated to budget:\n%.200s...", summary)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
