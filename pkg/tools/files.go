package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// Read 读取文件内容（offset/limit + 行号前缀，对应 pi 的 read 工具）。
func Read() agent.AgentTool {
	return &readTool{baseTool{
		name: "read",
		tool: llm.Tool{
			Name:        "read",
			Description: "Read a file with line numbers. Use offset/limit for large files.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   stringProp("Absolute or relative file path"),
					"offset": intProp("1-based line number to start from (default 1)"),
					"limit":  intProp("Max lines to read (default 2000)"),
				},
				"required": []any{"path"},
			},
		},
	}}
}

type readTool struct{ baseTool }

func (t *readTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if p.Offset <= 0 {
		p.Offset = 1
	}
	if p.Limit <= 0 {
		p.Limit = 2000
	}

	f, err := os.Open(p.Path)
	if err != nil {
		return textResult("error: " + err.Error()), nil
	}
	defer f.Close()

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	line := 0
	read := 0
	for scanner.Scan() {
		line++
		if line < p.Offset {
			continue
		}
		if read >= p.Limit {
			fmt.Fprintf(&b, "(stopped at limit %d; file has more lines)\n", p.Limit)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, scanner.Text())
		read++
	}
	if err := scanner.Err(); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	return textResult(b.String()), nil
}

// Write 写入文件（经 FileMutationQueue 串行化，对应 pi 的 write 工具）。
func Write() agent.AgentTool {
	return &writeTool{baseTool{
		name: "write",
		mode: agent.ExecutionSequential,
		tool: llm.Tool{
			Name:        "write",
			Description: "Write content to a file, creating or overwriting it. Parent directories are created.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    stringProp("File path to write"),
					"content": stringProp("Full content to write"),
				},
				"required": []any{"path", "content"},
			},
		},
	}}
}

type writeTool struct{ baseTool }

func (t *writeTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}

	FileMutationQueue.Lock()
	defer FileMutationQueue.Unlock()

	if err := os.MkdirAll(dirOf(p.Path), 0o755); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(p.Content), p.Path)), nil
}

func dirOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "."
	}
	return path[:idx]
}

// Edit 精确字符串替换（对应 pi 的 edit 工具语义：old_string 必须唯一匹配）。
func Edit() agent.AgentTool {
	return &editTool{baseTool{
		name: "edit",
		mode: agent.ExecutionSequential,
		tool: llm.Tool{
			Name:        "edit",
			Description: "Replace an exact string in a file. old_string must match exactly once unless replace_all is set.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":        stringProp("File path to edit"),
					"old_string":  stringProp("Exact string to replace"),
					"new_string":  stringProp("Replacement string"),
					"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences"},
				},
				"required": []any{"path", "old_string", "new_string"},
			},
		},
	}}
}

type editTool struct{ baseTool }

func (t *editTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if p.OldString == p.NewString {
		return textResult("error: old_string and new_string are identical"), nil
	}

	FileMutationQueue.Lock()
	defer FileMutationQueue.Unlock()

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return textResult("error: " + err.Error()), nil
	}
	content := normalizeCRLF(string(data))
	old := normalizeCRLF(p.OldString)
	newStr := normalizeCRLF(p.NewString)

	count := strings.Count(content, old)
	if count == 0 {
		return textResult("error: old_string not found in file"), nil
	}
	if count > 1 && !p.ReplaceAll {
		return textResult(fmt.Sprintf("error: old_string matches %d times; provide more context or set replace_all", count)), nil
	}

	var updated string
	if p.ReplaceAll {
		updated = strings.ReplaceAll(content, old, newStr)
	} else {
		updated = strings.Replace(content, old, newStr, 1)
	}
	if err := os.WriteFile(p.Path, []byte(updated), 0o644); err != nil {
		return textResult("error: " + err.Error()), nil
	}
	return textResult(fmt.Sprintf("edited %s (%d replacement(s))", p.Path, map[bool]int{true: count, false: 1}[p.ReplaceAll])), nil
}

var _ = bytes.MinRead // 占位避免未用导入
