package permission_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"goagent/pkg/permission"
)

func TestPolicyEvaluate(t *testing.T) {
	mutating := permission.Request{ToolName: "bash", ToolDecision: permission.AskUser("approve?")}
	readOnly := permission.Request{ToolName: "read", ReadOnly: true, ToolDecision: permission.Allowed()}

	tests := []struct {
		name     string
		policy   *permission.Policy
		request  permission.Request
		expected permission.Behavior
	}{
		{name: "default allows read only", policy: &permission.Policy{}, request: readOnly, expected: permission.Allow},
		{name: "default denies mutating without approver", policy: &permission.Policy{}, request: mutating, expected: permission.Deny},
		{name: "accept edits allows write", policy: &permission.Policy{Mode: permission.ModeAcceptEdits}, request: permission.Request{ToolName: "Write"}, expected: permission.Allow},
		{name: "accept edits allows multi edit", policy: &permission.Policy{Mode: permission.ModeAcceptEdits}, request: permission.Request{ToolName: "multi_edit"}, expected: permission.Allow},
		{name: "accept edits does not allow bash", policy: &permission.Policy{Mode: permission.ModeAcceptEdits}, request: mutating, expected: permission.Deny},
		{name: "plan allows read only", policy: &permission.Policy{Mode: permission.ModePlan}, request: readOnly, expected: permission.Allow},
		{name: "plan overrides allow rule", policy: &permission.Policy{Mode: permission.ModePlan, AllowedTools: []string{"bash"}}, request: mutating, expected: permission.Deny},
		{name: "dont ask allows listed tool", policy: &permission.Policy{Mode: permission.ModeDontAsk, AllowedTools: []string{"BASH"}}, request: mutating, expected: permission.Allow},
		{name: "dont ask denies unlisted tool", policy: &permission.Policy{Mode: permission.ModeDontAsk}, request: mutating, expected: permission.Deny},
		{name: "bypass allows mutating", policy: &permission.Policy{Mode: permission.ModeBypass}, request: mutating, expected: permission.Allow},
		{name: "deny rule beats bypass", policy: &permission.Policy{Mode: permission.ModeBypass, DisallowedTools: []string{"bash"}}, request: mutating, expected: permission.Deny},
		{name: "wildcard deny wins", policy: &permission.Policy{Mode: permission.ModeBypass, DisallowedTools: []string{"*"}}, request: readOnly, expected: permission.Deny},
		{name: "tool hard deny beats bypass", policy: &permission.Policy{Mode: permission.ModeBypass}, request: permission.Request{ToolName: "bash", ToolDecision: permission.Denied("unsafe")}, expected: permission.Deny},
		{name: "tool self allow", policy: &permission.Policy{}, request: permission.Request{ToolName: "custom", ToolDecision: permission.Allowed()}, expected: permission.Allow},
		{name: "invalid mode fails closed", policy: &permission.Policy{Mode: "unexpected"}, request: readOnly, expected: permission.Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := tt.policy.Evaluate(context.Background(), tt.request)
			if decision.Behavior != tt.expected {
				t.Fatalf("got behavior %q, want %q (decision: %+v)", decision.Behavior, tt.expected, decision)
			}
			if tt.expected == permission.Deny && decision.Reason == "" {
				t.Fatal("denial must include a reason")
			}
		})
	}
}

func TestPolicyApprover(t *testing.T) {
	request := permission.Request{ToolName: "bash"}
	tests := []struct {
		name     string
		response permission.Decision
		expected permission.Behavior
	}{
		{name: "allow", response: permission.Allowed(), expected: permission.Allow},
		{name: "deny", response: permission.Denied("not today"), expected: permission.Deny},
		{name: "ask again fails closed", response: permission.AskUser("again"), expected: permission.Deny},
		{name: "empty fails closed", response: permission.Decision{}, expected: permission.Deny},
		{name: "unknown fails closed", response: permission.Decision{Behavior: "maybe"}, expected: permission.Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			policy := permission.Policy{Approver: func(ctx context.Context, req permission.Request) permission.Decision {
				calls++
				if req.ToolName != request.ToolName {
					t.Fatalf("unexpected request: %+v", req)
				}
				return tt.response
			}}
			decision := policy.Evaluate(context.Background(), request)
			if decision.Behavior != tt.expected || calls != 1 {
				t.Fatalf("decision=%+v calls=%d", decision, calls)
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	for input, expected := range map[string]permission.Mode{
		"": permission.ModeDefault, "default": permission.ModeDefault,
		"acceptEdits": permission.ModeAcceptEdits, "ACCEPTEDITS": permission.ModeAcceptEdits,
		"plan": permission.ModePlan, "dontAsk": permission.ModeDontAsk,
		"bypass": permission.ModeBypass, "bypassPermissions": permission.ModeBypass,
	} {
		actual, err := permission.ParseMode(input)
		if err != nil || actual != expected {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	if _, err := permission.ParseMode("invalid"); err == nil || !strings.Contains(err.Error(), "unknown permission mode") {
		t.Fatalf("expected invalid mode error, got %v", err)
	}
}

func TestPolicySetMode(t *testing.T) {
	policy := &permission.Policy{}
	if mode := policy.CurrentMode(); mode != permission.ModeDefault {
		t.Fatalf("initial mode=%q, want default", mode)
	}
	if err := policy.SetMode(permission.ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	if mode := policy.CurrentMode(); mode != permission.ModeAcceptEdits {
		t.Fatalf("mode=%q, want acceptEdits", mode)
	}
	if err := policy.SetMode("invalid"); err == nil {
		t.Fatal("expected invalid mode error")
	}
	if mode := policy.CurrentMode(); mode != permission.ModeAcceptEdits {
		t.Fatalf("invalid update changed mode to %q", mode)
	}
}

func TestPolicyConcurrentModeSwitch(t *testing.T) {
	policy := &permission.Policy{}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				decision := policy.Evaluate(context.Background(), permission.Request{ToolName: "bash"})
				if decision.Behavior != permission.Allow && decision.Behavior != permission.Deny {
					t.Errorf("unexpected decision during mode switch: %+v", decision)
				}
			}
		}()
	}
	modes := []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModePlan, permission.ModeBypass}
	for i := 0; i < 100; i++ {
		if err := policy.SetMode(modes[i%len(modes)]); err != nil {
			t.Error(err)
		}
		_ = policy.CurrentMode()
	}
	wg.Wait()
}
