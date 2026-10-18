package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"

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
	return func(ctx context.Context, req permission.Request) permission.Decision {
		if err := ctx.Err(); err != nil {
			return permission.Denied("operation aborted")
		}
		fmt.Printf("\nAllow %s %s ? [y/N] ", req.ToolName, truncate(string(req.Args), 120))
		line, err := reader.ReadString('\n')
		if err != nil {
			return permission.Denied("approval input unavailable")
		}
		if ctx.Err() != nil {
			return permission.Denied("operation aborted")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return permission.Allowed()
		default:
			return permission.Denied("denied by user")
		}
	}
}

func nonInteractiveApprover(context.Context, permission.Request) permission.Decision {
	return permission.Denied("approval is unavailable in print mode; use -yes, -allow, or an appropriate -permission mode to grant access explicitly")
}
