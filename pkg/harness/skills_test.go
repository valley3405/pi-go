package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/llm"
)

func writeSkill(t *testing.T, root, dir, name, desc string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n\nSkill body.\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLoadSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "commit", "commit", "Write good commit messages")
	writeSkill(t, root, "nested/review", "review", "Review code")

	skills, warnings, err := harness.LoadSkills(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d: %v", len(skills), skills)
	}
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
		if s.Source != "project" {
			t.Fatalf("wrong source: %s", s.Source)
		}
	}
	if !names["commit"] || !names["review"] {
		t.Fatalf("missing skills: %v", names)
	}
}

func TestLoadSkillsRespectsIgnore(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "visible", "visible", "yes")
	writeSkill(t, root, "hidden", "hidden", "no")
	// .gitignore 忽略 hidden/
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("hidden/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 点开头的目录默认跳过
	writeSkill(t, root, ".secret", "secret", "no")

	skills, _, err := harness.LoadSkills(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "visible" {
		t.Fatalf("ignore rules not respected: %v", skills)
	}
}

func TestSkillFrontmatterDefaultsAndLimits(t *testing.T) {
	root := t.TempDir()
	// 无 name 时用目录名
	writeSkill(t, root, "mydir", "", "desc")
	// 无 frontmatter 的 SKILL.md 应被跳过并产生警告
	bad := filepath.Join(root, "bad")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter here"), 0o644)

	skills, warnings, err := harness.LoadSkills(root, "global")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "mydir" {
		t.Fatalf("unexpected skills: %v", skills)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "bad") {
		t.Fatalf("broken skill should surface a warning: %v", warnings)
	}

	// name 超长应被拒绝并产生警告
	longName := strings.Repeat("x", harness.MaxSkillNameLength+1)
	writeSkill(t, root, "toolong", longName, "desc")
	skills, warnings, _ = harness.LoadSkills(root, "global")
	for _, s := range skills {
		if s.Name == longName {
			t.Fatal("over-length skill name should be rejected")
		}
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %v", warnings)
	}
}

func TestMergeSkillsProjectOverridesGlobal(t *testing.T) {
	global := []harness.Skill{{Name: "commit", Description: "global version", Source: "global"}}
	project := []harness.Skill{{Name: "commit", Description: "project version", Source: "project"}}
	merged := harness.MergeSkills(global, project)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged skill, got %d", len(merged))
	}
	if merged[0].Description != "project version" || merged[0].Source != "project" {
		t.Fatalf("project should override global: %+v", merged[0])
	}
}

func TestFormatForSystemPrompt(t *testing.T) {
	out := harness.FormatForSystemPrompt([]harness.Skill{
		{Name: "commit", Dir: "/x/commit", Description: "Write good commit messages"},
	})
	if !strings.Contains(out, "# Available Skills") ||
		!strings.Contains(out, "commit") ||
		!strings.Contains(out, filepath.Join("/x/commit", "SKILL.md")) {
		t.Fatalf("bad prompt section:\n%s", out)
	}
	if harness.FormatForSystemPrompt(nil) != "" {
		t.Fatal("empty skills should produce empty string")
	}
}

func TestHarnessApplySkillsInjectsSystemPrompt(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{llm.FauxAssistant("ok")}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	a := agent.New(agent.Options{SystemPrompt: "base prompt", Model: model, StreamFn: reg.StreamSimple})
	h := harness.New(a, nil, reg.StreamSimple, llm.StreamOptions{})

	globalDir := t.TempDir()
	writeSkill(t, globalDir, "commit", "commit", "Write good commit messages")
	h.ApplySkills(harness.SkillDir{Root: globalDir, Source: "global"})

	sp := a.Snapshot().SystemPrompt
	if !strings.Contains(sp, "base prompt") || !strings.Contains(sp, "Available Skills") || !strings.Contains(sp, "commit") {
		t.Fatalf("skills not injected into system prompt:\n%s", sp)
	}
	if len(h.SkillsList()) != 1 {
		t.Fatalf("skills not populated: %v", h.SkillsList())
	}

	// 注入后的系统提示词应随 LLM 请求发出
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if len(faux.Requests) == 0 || !strings.Contains(faux.Requests[0].SystemPrompt, "Available Skills") {
		t.Fatal("LLM request should carry the skills section")
	}
}

func TestApplySkillsIsIdempotentAndReloadable(t *testing.T) {
	faux := llm.NewFauxProvider("faux", nil, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{SystemPrompt: "base prompt", Model: model, StreamFn: reg.StreamSimple})
	h := harness.New(a, nil, reg.StreamSimple, llm.StreamOptions{})

	dir := t.TempDir()
	writeSkill(t, dir, "one", "one", "first")
	dirs := harness.SkillDir{Root: dir, Source: "global"}
	h.ApplySkills(dirs)
	h.ApplySkills(dirs)
	if got := strings.Count(a.Snapshot().SystemPrompt, "# Available Skills"); got != 1 {
		t.Fatalf("skills section duplicated: %d occurrences\n%s", got, a.Snapshot().SystemPrompt)
	}

	// 新增技能后热重载
	writeSkill(t, dir, "two", "two", "second")
	h.ReloadSkills()
	sp := a.Snapshot().SystemPrompt
	if !strings.Contains(sp, "one") || !strings.Contains(sp, "two") || strings.Count(sp, "# Available Skills") != 1 {
		t.Fatalf("reload did not refresh skills section:\n%s", sp)
	}
	if len(h.SkillsList()) != 2 {
		t.Fatalf("reload skills = %v", h.SkillsList())
	}
}

func TestSkillToolLoadsAndLists(t *testing.T) {
	faux := llm.NewFauxProvider("faux", nil, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{SystemPrompt: "base", Model: model, StreamFn: reg.StreamSimple})
	h := harness.New(a, nil, reg.StreamSimple, llm.StreamOptions{})

	dir := t.TempDir()
	writeSkill(t, dir, "commit", "commit", "Write good commit messages")
	h.ApplySkills(harness.SkillDir{Root: dir, Source: "global"})

	var skill agent.AgentTool
	for _, tool := range a.Tools() {
		if tool.Name() == "skill" {
			skill = tool
		}
	}
	if skill == nil {
		t.Fatal("skill tool not registered")
	}
	// 只读标记：默认权限模式下自动放行，不触发审批
	readOnly, ok := skill.(interface{ IsReadOnly(json.RawMessage) bool })
	if !ok || !readOnly.IsReadOnly(nil) {
		t.Fatal("skill tool should be read-only")
	}

	textOf := func(res agent.ToolResult) string {
		return res.Content[0].(*llm.TextContent).Text
	}
	// 空名列出技能
	res, err := skill.Execute(context.Background(), "c1", json.RawMessage(`{"name":""}`), nil)
	if err != nil || !strings.Contains(textOf(res), "commit") {
		t.Fatalf("list failed: %+v %v", res, err)
	}
	// 按名加载正文（去掉 frontmatter）
	res, err = skill.Execute(context.Background(), "c2", json.RawMessage(`{"name":"commit"}`), nil)
	if err != nil || !strings.Contains(textOf(res), "Skill body.") || strings.Contains(textOf(res), "frontmatter") {
		t.Fatalf("load failed: %s", textOf(res))
	}
	// 未知技能报错并列出可用技能
	res, err = skill.Execute(context.Background(), "c3", json.RawMessage(`{"name":"nope"}`), nil)
	if err != nil || !strings.Contains(textOf(res), "unknown skill") || !strings.Contains(textOf(res), "commit") {
		t.Fatalf("unknown skill handling: %s", textOf(res))
	}
}

func TestProjectSkillsMarkedUntrusted(t *testing.T) {
	out := harness.FormatForSystemPrompt([]harness.Skill{
		{Name: "g", Dir: "/g", Source: "global", Description: "d"},
		{Name: "p", Dir: "/p", Source: "project", Description: "d"},
	})
	if !strings.Contains(out, "untrusted") || !strings.Contains(out, "skill tool") {
		t.Fatalf("prompt should mark project skills untrusted and reference the skill tool:\n%s", out)
	}
}
