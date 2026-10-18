package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// MultiEdit 在同一个文件中按顺序应用多组精确替换。
func MultiEdit() agent.AgentTool {
	return &multiEditTool{baseTool{
		name: "multi_edit",
		mode: agent.ExecutionSequential,
		tool: llm.Tool{
			Name:        "multi_edit",
			Description: "Apply multiple exact-string edits to one file in order. No changes are written unless every edit succeeds.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("File path to edit"),
					"edits": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"old_string":  stringProp("Exact string to replace"),
								"new_string":  stringProp("Replacement string"),
								"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences"},
							},
							"required": []any{"old_string", "new_string"},
						},
					},
				},
				"required": []any{"path", "edits"},
			},
		},
	}}
}

type multiEditTool struct{ baseTool }

type multiEditOperation struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *multiEditTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Path  string               `json:"path"`
		Edits []multiEditOperation `json:"edits"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if len(p.Edits) == 0 {
		return textResult("error: edits must contain at least one operation"), nil
	}

	FileMutationQueue.Lock()
	defer FileMutationQueue.Unlock()

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return textResult("error: " + err.Error()), nil
	}
	content := normalizeCRLF(string(data))
	totalReplacements := 0
	for i, edit := range p.Edits {
		oldText := normalizeCRLF(edit.OldString)
		newText := normalizeCRLF(edit.NewString)
		if oldText == "" {
			return textResult(fmt.Sprintf("error: edit %d: old_string must not be empty", i+1)), nil
		}
		if oldText == newText {
			return textResult(fmt.Sprintf("error: edit %d: old_string and new_string are identical", i+1)), nil
		}
		count := strings.Count(content, oldText)
		if count == 0 {
			return textResult(fmt.Sprintf("error: edit %d: old_string not found", i+1)), nil
		}
		if count > 1 && !edit.ReplaceAll {
			return textResult(fmt.Sprintf("error: edit %d: old_string matches %d times; provide more context or set replace_all", i+1, count)), nil
		}
		if edit.ReplaceAll {
			content = strings.ReplaceAll(content, oldText, newText)
			totalReplacements += count
		} else {
			content = strings.Replace(content, oldText, newText, 1)
			totalReplacements++
		}
	}
	if err := os.WriteFile(p.Path, []byte(content), 0o644); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	return textResult(fmt.Sprintf("edited %s (%d edit(s), %d replacement(s))", p.Path, len(p.Edits), totalReplacements)), nil
}
