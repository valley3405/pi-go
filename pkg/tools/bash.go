package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// BashResultDetails 描述进程退出状态，不改变返回给模型的文本内容。
type BashResultDetails struct {
	ExitCode int  `json:"exitCode"`
	Success  bool `json:"success"`
}

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

	// 防卡住三件套（修复"管道残留进程拖住 stdout 导致 Wait 不返回"）：
	// 1. Setpgid：子进程独立成组，后台/管道孙进程都在组内；
	// 2. Cancel：超时时整组 SIGKILL，而不是只杀 bash 本体；
	// 3. WaitDelay：命令退出后若仍有残留进程占着管道，最多再等 5s
	//    就强制关闭管道，Wait 保证返回（输出可能不完整，标注错误）。
	setProcGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = 5 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := truncateOutput(buf.String(), 100*1024)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		out += fmt.Sprintf("\n(command timed out after %ds, process group killed)", p.Timeout)
	case errors.Is(err, exec.ErrWaitDelay):
		out += "\n(command exited but leftover background processes held the pipes; output may be incomplete)"
	case err != nil:
		if exitErr, ok := err.(*exec.ExitError); ok {
			out += fmt.Sprintf("\n(exit code %d)", exitErr.ExitCode())
		} else {
			return agent.ToolResult{}, err
		}
	}
	if out == "" {
		out = "(no output)"
	}
	success := err == nil || errors.Is(err, exec.ErrWaitDelay)
	if ctx.Err() != nil {
		success = false
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if success {
		exitCode = 0
	}
	result := textResult(out)
	result.Details = BashResultDetails{ExitCode: exitCode, Success: success}
	return result, nil
}
