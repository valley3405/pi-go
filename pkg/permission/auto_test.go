package permission_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"goagent/pkg/permission"
)

func TestAutoModeTools(t *testing.T) {
	policy := &permission.Policy{Mode: permission.ModeAuto}
	if decision := policy.Evaluate(context.Background(), permission.Request{ToolName: "write", ToolDecision: permission.AskUser("approve?")}); decision.Behavior != permission.Allow {
		t.Fatalf("workspace edit was not automatic: %+v", decision)
	}
	calls := 0
	policy.Approver = func(context.Context, permission.Request) permission.Decision {
		calls++
		return permission.Denied("denied")
	}
	if decision := policy.Evaluate(context.Background(), permission.Request{ToolName: "custom", ToolDecision: permission.AskUser("approve?")}); decision.Behavior != permission.Deny || calls != 1 {
		t.Fatalf("custom tool did not require approval: %+v calls=%d", decision, calls)
	}
}

func TestAutoModeBashClassification(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"go test ./...",
		"gofmt -w main.go",
		"git status --short",
		"FOO=bar go test",
		"env GOFLAGS=-tags=test go test",
		"rm -rf src",
		"mv src src2",
		"sed -i 's/a/b/' src/file.go",
		"find . -delete",
		"find -L . -delete",
		"find -O2 . -delete",
	}
	for _, command := range allowed {
		t.Run("allow/"+command, func(t *testing.T) {
			calls := 0
			policy := &permission.Policy{Mode: permission.ModeAuto, WorkingDir: workspace, Approver: func(context.Context, permission.Request) permission.Decision {
				calls++
				return permission.Denied("denied")
			}}
			if decision := policy.Evaluate(context.Background(), bashRequest(t, command)); decision.Behavior != permission.Allow || calls != 0 {
				t.Fatalf("decision=%+v approvals=%d", decision, calls)
			}
		})
	}

	asked := []string{
		"rm -rf ../outside",
		"mv src /tmp/outside-goagent",
		"curl https://example.com",
		"sudo true",
		"git commit -m test",
		"git push origin main",
		"npm install",
		"unknown-command",
		"go test ./... && rm -rf .",
		"go test $(pwd)",
		"go test ./... > result.txt",
		"find -L /etc -delete",
		"find . -fprintf /tmp/out %p",
		"find . -okdir rm target",
		"cp --target-directory=/tmp src/file.go",
		"cp -t/tmp src/file.go",
		"mv --target-directory=/etc src",
	}
	for _, command := range asked {
		t.Run("ask/"+command, func(t *testing.T) {
			calls := 0
			policy := &permission.Policy{Mode: permission.ModeAuto, WorkingDir: workspace, Approver: func(context.Context, permission.Request) permission.Decision {
				calls++
				return permission.Denied("denied")
			}}
			if decision := policy.Evaluate(context.Background(), bashRequest(t, command)); decision.Behavior != permission.Deny || calls != 1 {
				t.Fatalf("decision=%+v approvals=%d", decision, calls)
			}
		})
	}
}

func TestAutoModeWorkspaceSymlinkTarget(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	policy := &permission.Policy{Mode: permission.ModeAuto, WorkingDir: workspace, Approver: func(context.Context, permission.Request) permission.Decision {
		calls++
		return permission.Denied("denied")
	}}
	if decision := policy.Evaluate(context.Background(), bashRequest(t, "rm link")); decision.Behavior != permission.Deny || calls != 1 {
		t.Fatalf("symlink escape did not require approval: %+v calls=%d", decision, calls)
	}
}

func TestAutoModeRequiredApprovalAndDeny(t *testing.T) {
	calls := 0
	policy := &permission.Policy{
		Mode:           permission.ModeAuto,
		DeniedCommands: []string{`go\s+test`},
		Approver: func(context.Context, permission.Request) permission.Decision {
			calls++
			return permission.Allowed()
		},
	}
	if decision := policy.Evaluate(context.Background(), bashRequest(t, "go test ./...")); decision.Behavior != permission.Deny || calls != 0 {
		t.Fatalf("deny command did not win: %+v calls=%d", decision, calls)
	}

	policy.DeniedCommands = nil
	request := bashRequest(t, "go test ./...")
	request.ToolDecision = permission.RequiredApproval("must review")
	if decision := policy.Evaluate(context.Background(), request); decision.Behavior != permission.Allow || calls != 1 {
		t.Fatalf("required approval did not ask: %+v calls=%d", decision, calls)
	}
}
