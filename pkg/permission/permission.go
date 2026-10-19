// Package permission 提供独立于 Agent 与 UI 的工具权限模式、规则和审批管线。
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
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
	// RequireApproval 禁止只读、allow 规则和 bypass 自动放行敏感访问。
	RequireApproval bool
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

// RequiredApproval 构造不能被自动允许规则覆盖的单次审批。
func RequiredApproval(reason string) Decision {
	return Decision{Behavior: Ask, Reason: reason, RequireApproval: true}
}

// Mode 控制权限管线的自动允许和审批行为。
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModeAuto        Mode = "auto"
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
	case "auto":
		return ModeAuto, nil
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
	mu              sync.RWMutex
	Mode            Mode
	AllowedTools    []string
	DisallowedTools []string
	// AllowedCommands 是完整命令正则；DeniedCommands 是部分匹配正则。
	// 含 shell 控制符、重定向或命令替换的命令不会被 allow 规则自动放行。
	AllowedCommands []string
	DeniedCommands  []string
	// WorkingDir 是 auto 模式判断工作区内删除/移动/修改命令的根目录；为空时使用当前目录。
	WorkingDir string
	Approver   Approver
}

// SetMode 在运行时切换权限模式，可与 Evaluate 并发调用。
func (p *Policy) SetMode(mode Mode) error {
	parsed, err := ParseMode(string(mode))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.Mode = parsed
	p.mu.Unlock()
	return nil
}

// CurrentMode 返回当前权限模式。
func (p *Policy) CurrentMode() Mode {
	p.mu.RLock()
	mode := p.Mode
	p.mu.RUnlock()
	if mode == "" {
		return ModeDefault
	}
	return mode
}

// Evaluate 按 deny 优先的顺序评估权限。工具规则支持 * 或精确工具名；
// Bash 命令规则中 allow 为完整匹配，deny 为部分匹配。
func (p *Policy) Evaluate(ctx context.Context, req Request) Decision {
	p.mu.RLock()
	mode := p.Mode
	allowedTools := append([]string(nil), p.AllowedTools...)
	disallowedTools := append([]string(nil), p.DisallowedTools...)
	allowedCommands := append([]string(nil), p.AllowedCommands...)
	deniedCommands := append([]string(nil), p.DeniedCommands...)
	workingDir := p.WorkingDir
	approver := p.Approver
	p.mu.RUnlock()

	if matchRule(disallowedTools, req.ToolName) {
		return Denied(fmt.Sprintf("tool %q is disallowed by policy", req.ToolName))
	}
	commandAllowed := false
	if command, isBash := bashCommand(req); isBash {
		denied, allowed, err := matchCommandRules(command, allowedCommands, deniedCommands)
		if err != nil {
			return Denied(err.Error())
		}
		if denied {
			return Denied(fmt.Sprintf("bash command is disallowed by policy: %q", command))
		}
		commandAllowed = allowed
	}
	if req.ToolDecision.Behavior == Deny {
		return Denied(req.ToolDecision.Reason)
	}

	if mode == "" {
		mode = ModeDefault
	}
	switch mode {
	case ModeBypass:
		if !req.ToolDecision.RequireApproval {
			return Allowed()
		}
	case ModePlan:
		if !req.ReadOnly {
			return Denied("plan mode permits read-only tools only")
		}
		if !req.ToolDecision.RequireApproval {
			return Allowed()
		}
	case ModeDefault, ModeAcceptEdits, ModeAuto, ModeDontAsk:
	default:
		return Denied(fmt.Sprintf("unknown permission mode %q", mode))
	}
	if req.ToolDecision.RequireApproval {
		return approve(ctx, req, mode, approver)
	}

	if matchRule(allowedTools, req.ToolName) {
		return Allowed()
	}
	if mode == ModeAcceptEdits && isEditTool(req.ToolName) {
		return Allowed()
	}
	if commandAllowed {
		return Allowed()
	}
	if req.ReadOnly {
		return Allowed()
	}
	if req.ToolDecision.Behavior == Allow {
		return Allowed()
	}
	if mode == ModeAuto {
		decision := autoDecision(req, workingDir)
		if decision.Behavior == Ask {
			req.ToolDecision = decision
			return approve(ctx, req, mode, approver)
		}
		return decision
	}
	return approve(ctx, req, mode, approver)
}

func approve(ctx context.Context, req Request, mode Mode, approver Approver) Decision {
	if ctx.Err() != nil {
		return Denied("operation aborted")
	}
	if mode == ModeDontAsk {
		return Denied(fmt.Sprintf("tool %q requires approval but mode is dontAsk", req.ToolName))
	}
	if approver == nil {
		return Denied(fmt.Sprintf("tool %q requires approval but no approval handler is configured", req.ToolName))
	}
	decision := approver(ctx, req)
	if ctx.Err() != nil {
		return Denied("operation aborted")
	}
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
	return strings.EqualFold(toolName, "write") || strings.EqualFold(toolName, "edit") || strings.EqualFold(toolName, "multi_edit")
}

// ValidateCommandRules 在启动时校验 Bash 命令规则。
func ValidateCommandRules(allowed, denied []string) error {
	for i, pattern := range denied {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("deny command rule %d: %w", i+1, err)
		}
	}
	for i, pattern := range allowed {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		if _, err := regexp.Compile(anchorAllowCommand(pattern)); err != nil {
			return fmt.Errorf("allow command rule %d: %w", i+1, err)
		}
	}
	return nil
}

func bashCommand(req Request) (string, bool) {
	if !strings.EqualFold(req.ToolName, "bash") {
		return "", false
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return "", false
	}
	return strings.TrimSpace(args.Command), true
}

func matchCommandRules(command string, allowed, denied []string) (bool, bool, error) {
	for i, pattern := range denied {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false, false, fmt.Errorf("invalid deny command rule %d: %w", i+1, err)
		}
		if re.MatchString(command) {
			return true, false, nil
		}
	}
	if hasUnsafeCommandSyntax(command) {
		return false, false, nil
	}
	for i, pattern := range allowed {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		re, err := regexp.Compile(anchorAllowCommand(pattern))
		if err != nil {
			return false, false, fmt.Errorf("invalid allow command rule %d: %w", i+1, err)
		}
		if re.MatchString(command) {
			return false, true, nil
		}
	}
	return false, false, nil
}

func hasUnsafeCommandSyntax(command string) bool {
	var singleQuoted, doubleQuoted, escaped bool
	for _, r := range command {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if singleQuoted {
			if r == '\'' {
				singleQuoted = false
			}
			continue
		}
		if r == '\'' {
			singleQuoted = true
			continue
		}
		if doubleQuoted {
			if r == '"' {
				doubleQuoted = false
				continue
			}
			if r == '$' || r == '`' {
				return true
			}
			continue
		}
		switch r {
		case ';', '|', '&', '<', '>', '(', ')', '{', '}', '$', '`', '~', '#', '\n', '\r', '[', ']':
			return true
		}
	}
	return singleQuoted || doubleQuoted || escaped
}

func anchorAllowCommand(pattern string) string {
	return "^(?:" + pattern + ")$"
}
