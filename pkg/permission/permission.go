// Package permission 提供独立于 Agent 与 UI 的工具权限模式、规则和审批管线。
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Behavior 是权限决策结果。
type Behavior string

const (
	Allow Behavior = "allow"
	Deny  Behavior = "deny"
	Ask   Behavior = "ask"
)

// Decision 是工具或审批器返回的权限决策。
type Decision struct {
	Behavior Behavior
	Reason   string
}

// Allowed 构造允许决策。
func Allowed() Decision { return Decision{Behavior: Allow} }

// Denied 构造拒绝决策。
func Denied(reason string) Decision {
	if reason == "" {
		reason = "denied"
	}
	return Decision{Behavior: Deny, Reason: reason}
}

// AskUser 构造需要用户审批的决策。
func AskUser(reason string) Decision { return Decision{Behavior: Ask, Reason: reason} }

// Mode 控制权限管线的自动允许和审批行为。
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModePlan        Mode = "plan"
	ModeDontAsk     Mode = "dontAsk"
	ModeBypass      Mode = "bypassPermissions"
)

// ParseMode 解析 CLI 或配置中的权限模式。
func ParseMode(value string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return ModeDefault, nil
	case "acceptedits":
		return ModeAcceptEdits, nil
	case "plan":
		return ModePlan, nil
	case "dontask":
		return ModeDontAsk, nil
	case "bypass", "bypasspermissions":
		return ModeBypass, nil
	default:
		return "", fmt.Errorf("unknown permission mode %q", value)
	}
}

// Request 是一次工具权限评估的输入。
type Request struct {
	ToolName     string
	Args         json.RawMessage
	ReadOnly     bool
	ToolDecision Decision
}

// Approver 在权限管线进入 ask 阶段时作出最终决定。
type Approver func(ctx context.Context, req Request) Decision

// Evaluator 是 Agent 依赖的权限评估边界。
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) Decision
}

// Policy 组合权限模式、工具名规则和宿主审批器。
type Policy struct {
	Mode            Mode
	AllowedTools    []string
	DisallowedTools []string
	Approver        Approver
}

// Evaluate 按 deny 优先的顺序评估权限。规则支持 * 或精确工具名。
func (p Policy) Evaluate(ctx context.Context, req Request) Decision {
	if matchRule(p.DisallowedTools, req.ToolName) {
		return Denied(fmt.Sprintf("tool %q is disallowed by policy", req.ToolName))
	}
	if req.ToolDecision.Behavior == Deny {
		return Denied(req.ToolDecision.Reason)
	}

	mode := p.Mode
	if mode == "" {
		mode = ModeDefault
	}
	switch mode {
	case ModeBypass:
		return Allowed()
	case ModePlan:
		if !req.ReadOnly {
			return Denied("plan mode permits read-only tools only")
		}
		return Allowed()
	case ModeDefault, ModeAcceptEdits, ModeDontAsk:
	default:
		return Denied(fmt.Sprintf("unknown permission mode %q", mode))
	}

	if matchRule(p.AllowedTools, req.ToolName) {
		return Allowed()
	}
	if mode == ModeAcceptEdits && isEditTool(req.ToolName) {
		return Allowed()
	}
	if req.ReadOnly {
		return Allowed()
	}
	if req.ToolDecision.Behavior == Allow {
		return Allowed()
	}
	if mode == ModeDontAsk {
		return Denied(fmt.Sprintf("tool %q requires approval but mode is dontAsk", req.ToolName))
	}
	if p.Approver == nil {
		return Denied(fmt.Sprintf("tool %q requires approval but no approval handler is configured", req.ToolName))
	}
	decision := p.Approver(ctx, req)
	switch decision.Behavior {
	case Allow:
		return Allowed()
	case Deny:
		return Denied(decision.Reason)
	default:
		return Denied("approval handler did not make an allow or deny decision")
	}
}

func matchRule(rules []string, toolName string) bool {
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "*" || strings.EqualFold(rule, toolName) {
			return true
		}
	}
	return false
}

func isEditTool(toolName string) bool {
	return strings.EqualFold(toolName, "write") || strings.EqualFold(toolName, "edit")
}
