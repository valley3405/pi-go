// Package tools 是内置编码工具集（对应 pi 的 coding-agent/core/tools）。
// 所有工具实现 agent.AgentTool；写操作经 FileMutationQueue 串行化。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
	"goagent/pkg/permission"
)

// FileMutationQueue 串行化文件写操作（对应 pi 的 file-mutation-queue）。
// 并行工具执行时，write/edit 经此队列避免交错写坏文件。
var FileMutationQueue sync.Mutex

// baseTool 提供公共字段。
type baseTool struct {
	name     string
	tool     llm.Tool
	mode     agent.ExecutionMode
	readOnly bool
}

func (b *baseTool) Name() string     { return b.name }
func (b *baseTool) Schema() llm.Tool { return b.tool }
func (b *baseTool) ExecutionMode() agent.ExecutionMode {
	if b.mode == "" {
		return agent.ExecutionParallel
	}
	return b.mode
}
func (b *baseTool) IsReadOnly(json.RawMessage) bool { return b.readOnly }
func (b *baseTool) CheckPermissions(context.Context, json.RawMessage) permission.Decision {
	if b.readOnly {
		return permission.Allowed()
	}
	return permission.AskUser("approve " + b.name + "?")
}

func textResult(s string) agent.ToolResult {
	return agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: s}}}
}

func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return fmt.Errorf("missing arguments")
	}
	return json.Unmarshal(args, v)
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func normalizeCRLF(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// CodingTools 返回完整编码工具集（对应 pi 的 createCodingTools）。
func CodingTools() []agent.AgentTool {
	return []agent.AgentTool{Bash(), Read(), Write(), Edit(), MultiEdit(), LS(), Grep(), Glob()}
}

// ReadOnlyTools 返回只读工具集（对应 pi 的 createReadOnlyTools）。
func ReadOnlyTools() []agent.AgentTool {
	return []agent.AgentTool{Read(), LS(), Grep(), Glob()}
}
