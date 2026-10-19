package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"goagent/pkg/permission"
)

type ruleList []string

func (r *ruleList) String() string { return strings.Join(*r, ",") }

func (r *ruleList) Set(value string) error {
	for _, rule := range strings.Split(value, ",") {
		if rule = strings.TrimSpace(rule); rule != "" {
			*r = append(*r, rule)
		}
	}
	return nil
}

func newPermissionPolicy(mode string, yes bool, allowed, denied []string) (*permission.Policy, error) {
	if yes {
		mode = string(permission.ModeBypass)
	}
	parsed, err := permission.ParseMode(mode)
	if err != nil {
		return nil, err
	}
	return &permission.Policy{
		Mode:            parsed,
		AllowedTools:    append([]string(nil), allowed...),
		DisallowedTools: append([]string(nil), denied...),
	}, nil
}

func terminalApprover(reader *bufio.Reader) permission.Approver {
	var mu sync.Mutex
	return func(ctx context.Context, req permission.Request) permission.Decision {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return permission.Denied("operation aborted")
		}
		fmt.Println()
		if req.ToolDecision.RequireApproval {
			fmt.Println(req.ToolDecision.Reason)
		}
		fmt.Printf("审批请求：%s\n参数：\n%s\n\n  1) 允许\n  2) 拒绝\n请选择 [1/2]: ", req.ToolName, prettyArgs(req.Args))
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return permission.Denied("approval input unavailable")
			}
			if ctx.Err() != nil {
				return permission.Denied("operation aborted")
			}
			switch strings.TrimSpace(line) {
			case "1":
				return permission.Allowed()
			case "2", "":
				return permission.Denied("denied by user")
			default:
				fmt.Print("无效输入，请输入 1（允许）或 2（拒绝）: ")
			}
		}
	}
}

// prettyArgs prints the complete tool arguments, indented when valid JSON.
func prettyArgs(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "(无参数)"
	}
	var buf bytes.Buffer
	if json.Indent(&buf, []byte(trimmed), "", "  ") == nil {
		return buf.String()
	}
	return trimmed
}

func nonInteractiveApprover(_ context.Context, req permission.Request) permission.Decision {
	if req.ToolDecision.RequireApproval {
		return permission.Denied("approval is unavailable in print mode: " + req.ToolDecision.Reason)
	}
	return permission.Denied("approval is unavailable in print mode; use -yes, -allow, or an appropriate -permission mode to grant access explicitly")
}
