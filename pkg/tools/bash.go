package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// Bash 执行 shell 命令（对应 pi 的 bash 工具）：超时 + 输出截断。
func Bash() agent.AgentTool {
	return &bashTool{baseTool{
		name: "bash",
		tool: llm.Tool{
			Name:        "bash",
			Description: "Execute a bash command in the current working directory and return its stdout/stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": stringProp("The bash command to execute"),
					"timeout": intProp("Timeout in seconds (default 60, max 600)"),
				},
				"required": []any{"command"},
			},
		},
	}}
}

type bashTool struct{ baseTool }

func (t *bashTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if p.Timeout <= 0 {
		p.Timeout = 60
	}
	if p.Timeout > 600 {
		p.Timeout = 600
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", p.Command)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := truncateOutput(buf.String(), 100*1024)
	if ctx.Err() == context.DeadlineExceeded {
		out += fmt.Sprintf("\n(command timed out after %ds)", p.Timeout)
	} else if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			out += fmt.Sprintf("\n(exit code %d)", exitErr.ExitCode())
		} else {
			return agent.ToolResult{}, err
		}
	}
	if out == "" {
		out = "(no output)"
	}
	return textResult(out), nil
}
