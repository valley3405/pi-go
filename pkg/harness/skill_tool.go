package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valley3405/pi-go/pkg/agent"
	"github.com/valley3405/pi-go/pkg/llm"
)

// SkillResultDetails 是 skill 工具成功加载时附加到结果消息 Payload 的
// 元数据；compaction 据此找回最近一次加载的技能并重注入正文。
type SkillResultDetails struct {
	SkillName string `json:"skillName"`
	Dir       string `json:"dir"`
}

// skillTool 是只读的技能加载工具：模型按名加载 SKILL.md 全文，
// 不需要对工作区外的技能目录发起文件读取审批。
type skillTool struct {
	h *Harness
}

func (skillTool) Name() string { return "skill" }

func (skillTool) Schema() llm.Tool {
	return llm.Tool{
		Name: "skill",
		Description: "Load a skill by name to get its full instructions, then follow them. " +
			"Use the names from the Available Skills list; pass an empty name to list available skills.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "skill name to load; empty string lists available skills",
				},
			},
		},
	}
}

func (skillTool) ExecutionMode() agent.ExecutionMode { return agent.ExecutionParallel }

// IsReadOnly 标记为只读，使默认权限模式自动放行（不触发审批）。
func (skillTool) IsReadOnly(json.RawMessage) bool { return true }

func (t skillTool) Execute(_ context.Context, _ string, args json.RawMessage, _ func(agent.ToolResult)) (agent.ToolResult, error) {
	var parsed struct {
		Name string `json:"name"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return agent.ErrorToolResult("invalid skill arguments: " + err.Error()), nil
		}
	}
	name := strings.TrimSpace(parsed.Name)
	skills := t.h.SkillsList()
	if name == "" {
		return agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: skillListText(skills)}}}, nil
	}
	for _, s := range skills {
		if s.Name != name {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, skillFileName))
		if err != nil {
			return agent.ErrorToolResult(fmt.Sprintf("read skill %q: %v", name, err)), nil
		}
		text := fmt.Sprintf("# Skill: %s [%s]\n\n%s", s.Name, s.Dir, SkillBody(data))
		return agent.ToolResult{
			Content: []llm.Content{&llm.TextContent{Text: text}},
			Details: SkillResultDetails{SkillName: s.Name, Dir: s.Dir},
		}, nil
	}
	return agent.ErrorToolResult(fmt.Sprintf("unknown skill %q. Available skills:\n%s", name, skillListText(skills))), nil
}

func skillListText(skills []Skill) string {
	if len(skills) == 0 {
		return "(no skills available)"
	}
	var b strings.Builder
	for i, s := range skills {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- %s [%s]: %s", s.Name, s.Source, s.Description)
	}
	return b.String()
}
