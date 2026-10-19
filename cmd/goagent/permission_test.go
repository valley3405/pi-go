package main

import (
	"bufio"
	"context"
	"strings"
	"testing"

	"goagent/pkg/permission"
)

func TestRuleList(t *testing.T) {
	var rules ruleList
	if err := rules.Set("bash, read"); err != nil {
		t.Fatal(err)
	}
	if err := rules.Set("write"); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 || rules[0] != "bash" || rules[1] != "read" || rules[2] != "write" {
		t.Fatalf("unexpected rules: %v", rules)
	}
}

func TestNewPermissionPolicy(t *testing.T) {
	policy, err := newPermissionPolicy("acceptEdits", false, []string{"read"}, []string{"bash"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != permission.ModeAcceptEdits || len(policy.AllowedTools) != 1 || len(policy.DisallowedTools) != 1 {
		t.Fatalf("unexpected policy: %+v", policy)
	}
	policy, err = newPermissionPolicy("plan", true, nil, []string{"bash"}, []string{`go test( .*)?`}, []string{`rm\s+-rf`})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != permission.ModeBypass {
		t.Fatalf("-yes mode=%q, want bypass", policy.Mode)
	}
	if decision := policy.Evaluate(context.Background(), permission.Request{ToolName: "bash"}); decision.Behavior != permission.Deny {
		t.Fatalf("deny rule should beat -yes: %+v", decision)
	}
	if len(policy.AllowedCommands) != 1 || len(policy.DeniedCommands) != 1 {
		t.Fatalf("command rules missing: %+v", policy)
	}
	if _, err := newPermissionPolicy("invalid", false, nil, nil, nil, nil); err == nil {
		t.Fatal("expected invalid mode error")
	}
	if _, err := newPermissionPolicy("default", false, nil, nil, []string{"["}, nil); err == nil || !strings.Contains(err.Error(), "allow command rule") {
		t.Fatalf("expected invalid command rule error, got %v", err)
	}
}

func TestTerminalApprover(t *testing.T) {
	tests := []struct {
		input    string
		expected permission.Behavior
	}{
		{input: "1\n", expected: permission.Allow},
		{input: "y\n1\n", expected: permission.Allow}, // y alone is invalid, then explicit 1
		{input: "yes\n", expected: permission.Deny},   // yes alone no longer approves (EOF after invalid input)
		{input: "2\n", expected: permission.Deny},
		{input: "\n", expected: permission.Deny},
		{input: "", expected: permission.Deny},
	}
	for _, tt := range tests {
		approver := terminalApprover(bufio.NewReader(strings.NewReader(tt.input)))
		decision := approver(context.Background(), permission.Request{ToolName: "bash"})
		if decision.Behavior != tt.expected {
			t.Errorf("input %q: behavior=%q, want %q", tt.input, decision.Behavior, tt.expected)
		}
	}
}

func TestNonInteractiveApprover(t *testing.T) {
	decision := nonInteractiveApprover(context.Background(), permission.Request{ToolName: "write"})
	if decision.Behavior != permission.Deny || !strings.Contains(decision.Reason, "-yes") {
		t.Fatalf("unexpected print-mode decision: %+v", decision)
	}
}

func TestPrintCannotBypassOutsideReadApproval(t *testing.T) {
	policy, err := newPermissionPolicy("default", true, []string{"read"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy.Approver = nonInteractiveApprover
	decision := policy.Evaluate(context.Background(), permission.Request{ToolName: "read", ReadOnly: true, ToolDecision: permission.RequiredApproval("read outside workspace: /outside/file")})
	if decision.Behavior != permission.Deny || !strings.Contains(decision.Reason, "outside workspace") || strings.Contains(decision.Reason, "use -yes") {
		t.Fatalf("decision=%+v", decision)
	}
}

func TestCommandRuleListDoesNotSplitCommas(t *testing.T) {
	var rules commandRuleList
	if err := rules.Set(`^go test -run 'Test(A|B),TestC$'`); err != nil {
		t.Fatal(err)
	}
	if err := rules.Set(`^printf "hello"$`); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || !strings.Contains(rules[0], "TestC") {
		t.Fatalf("command rules split incorrectly: %v", rules)
	}
}
