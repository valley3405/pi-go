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

	skills, err := harness.LoadSkills(root, "project")
	if err != nil {
		t.Fatal(err)
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

	skills, err := harness.LoadSkills(root, "project")
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
	// 无 frontmatter 的 SKILL.md 应被跳过
	bad := filepath.Join(root, "bad")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter here"), 0o644)

	skills, err := harness.LoadSkills(root, "global")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "mydir" {
		t.Fatalf("unexpected skills: %v", skills)
	}

	// name 超长应被拒绝
	longName := strings.Repeat("x", harness.MaxSkillNameLength+1)
	writeSkill(t, root, "toolong", longName, "desc")
	skills, _ = harness.LoadSkills(root, "global")
	for _, s := range skills {
		if s.Name == longName {
			t.Fatal("over-length skill name should be rejected")
		}
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
	h.ApplySkills(globalDir, t.TempDir())

	sp := a.Snapshot().SystemPrompt
	if !strings.Contains(sp, "base prompt") || !strings.Contains(sp, "Available Skills") || !strings.Contains(sp, "commit") {
		t.Fatalf("skills not injected into system prompt:\n%s", sp)
	}
	if len(h.Skills) != 1 {
		t.Fatalf("harness.Skills not populated: %v", h.Skills)
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
