package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/permission"
)

func TestWorkspaceAutoMode(t *testing.T) {
	w, parent := testWorkspace(t)
	inside := filepath.Join(w.Root(), "changed.txt")
	putFile(t, inside, "before")
	outside := filepath.Join(parent, "outside.txt")
	putFile(t, outside, "outside")

	calls := 0
	policy := &permission.Policy{
		Mode:       permission.ModeAuto,
		WorkingDir: w.Root(),
		Approver: func(context.Context, permission.Request) permission.Decision {
			calls++
			return permission.Denied("test denial")
		},
	}

	result := workspaceCall(t, w, "edit", map[string]any{"path": "changed.txt", "old_string": "before", "new_string": "after"}, policy, nil)
	if result.IsError || calls != 0 {
		t.Fatalf("inside edit required approval: %s calls=%d", result.Text(), calls)
	}
	if data, _ := os.ReadFile(inside); string(data) != "after" {
		t.Fatalf("inside edit failed: %q", data)
	}

	result = workspaceCall(t, w, "bash", map[string]any{"command": "rm changed.txt"}, policy, nil)
	if result.IsError || calls != 0 {
		t.Fatalf("inside rm required approval: %s calls=%d", result.Text(), calls)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("inside rm failed: %v", err)
	}

	result = workspaceCall(t, w, "bash", map[string]any{"command": "rm ../outside.txt"}, policy, nil)
	if !result.IsError || calls != 1 {
		t.Fatalf("outside rm did not require approval: %s calls=%d", result.Text(), calls)
	}
	if data, _ := os.ReadFile(outside); string(data) != "outside" {
		t.Fatal("outside file changed")
	}

	result = workspaceCall(t, w, "read", map[string]any{"path": outside}, policy, nil)
	if !result.IsError || calls != 2 || strings.Contains(result.Text(), "outside") {
		t.Fatalf("outside read did not require approval: %s calls=%d", result.Text(), calls)
	}
}
