package permission_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/valley3405/pi-go/pkg/permission"
)

func bashRequest(t *testing.T, command string) permission.Request {
	t.Helper()
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return permission.Request{ToolName: "bash", Args: args, ToolDecision: permission.AskUser("approve bash?")}
}

func TestCommandAllowRulesUseFullMatch(t *testing.T) {
	policy := &permission.Policy{AllowedCommands: []string{`go test( .*)?`}}
	if decision := policy.Evaluate(context.Background(), bashRequest(t, "go test ./...")); decision.Behavior != permission.Allow {
		t.Fatalf("allowed command denied: %+v", decision)
	}
	for _, command := range []string{
		"go test ./... && rm -rf /tmp/x",
		"echo go test",
		"go test ./...; echo extra",
		"go test ./... | tee result.txt",
		"go test $(pwd)",
		"go test ./... > result.txt",
		"go test 'broken",
	} {
		if decision := policy.Evaluate(context.Background(), bashRequest(t, command)); decision.Behavior != permission.Deny {
			t.Fatalf("composite command %q unexpectedly allowed: %+v", command, decision)
		}
	}
	if decision := policy.Evaluate(context.Background(), bashRequest(t, "go test -run 'Test(A|B)'")); decision.Behavior != permission.Allow {
		t.Fatalf("quoted shell metacharacter should not block allow: %+v", decision)
	}
}

func TestCommandDenyRulesBeatBypassAndToolAllow(t *testing.T) {
	policy := &permission.Policy{
		Mode:           permission.ModeBypass,
		AllowedTools:   []string{"bash"},
		DeniedCommands: []string{`rm\s+-rf`},
	}
	decision := policy.Evaluate(context.Background(), bashRequest(t, "echo ok && rm -rf /tmp/x"))
	if decision.Behavior != permission.Deny || !strings.Contains(decision.Reason, "disallowed") {
		t.Fatalf("deny command did not win: %+v", decision)
	}
}

func TestCommandRulesModesAndRequiredApproval(t *testing.T) {
	request := bashRequest(t, "printf ok")
	plan := &permission.Policy{Mode: permission.ModePlan, AllowedCommands: []string{`printf ok`}}
	if decision := plan.Evaluate(context.Background(), request); decision.Behavior != permission.Deny {
		t.Fatalf("plan mode allowed bash: %+v", decision)
	}

	dontAsk := &permission.Policy{Mode: permission.ModeDontAsk, AllowedCommands: []string{`printf ok`}}
	if decision := dontAsk.Evaluate(context.Background(), request); decision.Behavior != permission.Allow {
		t.Fatalf("dontAsk allow rule failed: %+v", decision)
	}

	calls := 0
	required := &permission.Policy{
		AllowedCommands: []string{`printf ok`},
		Approver: func(context.Context, permission.Request) permission.Decision {
			calls++
			return permission.Allowed()
		},
	}
	request.ToolDecision = permission.RequiredApproval("must review")
	if decision := required.Evaluate(context.Background(), request); decision.Behavior != permission.Allow || calls != 1 {
		t.Fatalf("required approval bypassed: %+v calls=%d", decision, calls)
	}
}

func TestInvalidCommandRulesFailClosed(t *testing.T) {
	for name, policy := range map[string]*permission.Policy{
		"deny":  {DeniedCommands: []string{"["}},
		"allow": {AllowedCommands: []string{"["}},
	} {
		decision := policy.Evaluate(context.Background(), bashRequest(t, "printf ok"))
		if decision.Behavior != permission.Deny || !strings.Contains(decision.Reason, "invalid "+name+" command rule") {
			t.Fatalf("%s rule did not fail closed: %+v", name, decision)
		}
	}
	if err := permission.ValidateCommandRules([]string{"["}, nil); err == nil {
		t.Fatal("invalid allow rule accepted")
	}
	if err := permission.ValidateCommandRules(nil, []string{"["}); err == nil {
		t.Fatal("invalid deny rule accepted")
	}
}

func TestCommandRulesIgnoreNonBash(t *testing.T) {
	policy := &permission.Policy{AllowedCommands: []string{`.*`}, DeniedCommands: []string{`.*`}}
	request := permission.Request{ToolName: "write", ToolDecision: permission.AskUser("approve?")}
	decision := policy.Evaluate(context.Background(), request)
	if decision.Behavior != permission.Deny || !strings.Contains(decision.Reason, "no approval handler") {
		t.Fatalf("command rules affected non-bash tool: %+v", decision)
	}
}
