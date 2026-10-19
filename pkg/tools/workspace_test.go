//go:build unix

package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
	"goagent/pkg/permission"
	"goagent/pkg/tools"
	"golang.org/x/sys/unix"
)

func testWorkspace(t *testing.T) (*tools.Workspace, string) {
	t.Helper()
	parent := t.TempDir()
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	w, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, parent
}

func putFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func makeLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func workspaceCall(t *testing.T, w *tools.Workspace, name string, args map[string]any, policy permission.Evaluator, hook func()) agent.AgentMessage {
	t.Helper()
	faux := llm.NewFauxProvider("test", []llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "call", Name: name, Arguments: jargs(args)}),
		llm.FauxAssistant("done"),
	}, 4096)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("test", "faux-1")
	a := agent.New(agent.Options{Model: model, Tools: w.CodingTools(), StreamFn: reg.StreamSimple, Permission: policy})
	if hook != nil {
		a.BeforeToolCall = func(context.Context, agent.BeforeToolCallContext) (*agent.BeforeToolCallResult, error) {
			hook()
			return nil, nil
		}
	}
	if err := a.Prompt(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleToolResult {
			return msg
		}
	}
	t.Fatal("no paired tool result")
	return agent.AgentMessage{}
}

func TestWorkspaceOutsideReadsRequireApproval(t *testing.T) {
	for _, name := range []string{"read", "ls", "grep", "glob"} {
		for _, allow := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{false: "deny", true: "allow"}[allow], func(t *testing.T) {
				w, parent := testWorkspace(t)
				putFile(t, filepath.Join(parent, "outside.txt"), "OUTSIDE_MARKER")
				path := parent
				if name == "read" {
					path = filepath.Join(parent, "outside.txt")
				}
				calls := 0
				policy := &permission.Policy{Approver: func(ctx context.Context, req permission.Request) permission.Decision {
					calls++
					if !req.ReadOnly || !req.ToolDecision.RequireApproval || !strings.Contains(req.ToolDecision.Reason, path) {
						t.Errorf("unexpected approval: %+v", req)
					}
					if allow {
						return permission.Allowed()
					}
					return permission.Denied("user denied")
				}}
				pattern := "OUTSIDE_MARKER"
				if name == "glob" {
					pattern = "*.txt"
				}
				result := workspaceCall(t, w, name, map[string]any{"path": path, "pattern": pattern, "max_results": 20}, policy, nil)
				if calls != 1 || result.IsError != !allow {
					t.Fatalf("calls=%d result=%+v", calls, result)
				}
				if !allow && strings.Contains(result.Text(), "OUTSIDE_MARKER") {
					t.Fatal("outside contents leaked")
				}
				wantText := "OUTSIDE_MARKER"
				if name == "ls" || name == "glob" {
					wantText = "outside.txt"
				}
				if allow && !strings.Contains(result.Text(), wantText) {
					t.Fatalf("approved read failed: %s", result.Text())
				}
			})
		}
	}
}

func TestWorkspaceReadPathsAndModes(t *testing.T) {
	w, parent := testWorkspace(t)
	putFile(t, filepath.Join(w.Root(), "inside.txt"), "INSIDE")
	putFile(t, filepath.Join(parent, "outside.txt"), "OUTSIDE")
	makeLink(t, filepath.Join(parent, "outside.txt"), filepath.Join(w.Root(), "escape"))
	makeLink(t, filepath.Join(w.Root(), "inside.txt"), filepath.Join(w.Root(), "internal"))
	for _, path := range []string{"inside.txt", "internal", filepath.Join(w.Root(), "inside.txt")} {
		result := workspaceCall(t, w, "read", map[string]any{"path": path}, &permission.Policy{}, nil)
		if result.IsError || !strings.Contains(result.Text(), "INSIDE") {
			t.Fatalf("inside path %q: %s", path, result.Text())
		}
	}
	for _, mode := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModePlan, permission.ModeDontAsk, permission.ModeBypass} {
		for _, path := range []string{"../outside.txt", filepath.Join(parent, "outside.txt"), "escape"} {
			calls := 0
			policy := &permission.Policy{Mode: mode, AllowedTools: []string{"*"}, Approver: func(context.Context, permission.Request) permission.Decision {
				calls++
				return permission.Denied("test denial")
			}}
			result := workspaceCall(t, w, "read", map[string]any{"path": path}, policy, nil)
			wantCalls := 1
			if mode == permission.ModeDontAsk {
				wantCalls = 0
			}
			if !result.IsError || calls != wantCalls || strings.Contains(result.Text(), "OUTSIDE") {
				t.Fatalf("mode=%s path=%s calls=%d result=%s", mode, path, calls, result.Text())
			}
		}
	}
	result := workspaceCall(t, w, "read", map[string]any{"path": "../outside.txt"}, nil, nil)
	if !result.IsError || strings.Contains(result.Text(), "OUTSIDE") {
		t.Fatal("nil evaluator bypassed workspace boundary")
	}
}

func TestWorkspaceWritesStayInside(t *testing.T) {
	for _, name := range []string{"write", "edit", "multi_edit"} {
		t.Run(name, func(t *testing.T) {
			w, parent := testWorkspace(t)
			outside := filepath.Join(parent, "outside.txt")
			putFile(t, outside, "before")
			makeLink(t, outside, filepath.Join(w.Root(), "escape"))
			makeLink(t, parent, filepath.Join(w.Root(), "parent-escape"))
			args := map[string]any{"content": "after", "old_string": "before", "new_string": "after", "edits": []map[string]any{{"old_string": "before", "new_string": "after"}}}
			for _, path := range []string{"../outside.txt", outside, "escape", "parent-escape/new/file.txt", filepath.Join(parent, "project-backup", "file.txt")} {
				args["path"] = path
				result := workspaceCall(t, w, name, args, &permission.Policy{Mode: permission.ModeBypass, AllowedTools: []string{"*"}}, nil)
				if !result.IsError || !strings.Contains(result.Text(), "outside workspace") {
					t.Fatalf("path=%s result=%s", path, result.Text())
				}
			}
			data, _ := os.ReadFile(outside)
			if string(data) != "before" {
				t.Fatal("outside file changed")
			}
			inside := filepath.Join(w.Root(), "inside.txt")
			putFile(t, inside, "before")
			makeLink(t, inside, filepath.Join(w.Root(), "internal"))
			args["path"] = "internal"
			result := workspaceCall(t, w, name, args, &permission.Policy{Mode: permission.ModeAcceptEdits}, nil)
			data, _ = os.ReadFile(inside)
			if result.IsError || string(data) != "after" {
				t.Fatalf("inside edit failed: %s / %q", result.Text(), data)
			}
		})
	}
	w, _ := testWorkspace(t)
	result := workspaceCall(t, w, "write", map[string]any{"path": "new/sub/file.txt", "content": "created"}, &permission.Policy{Mode: permission.ModeAcceptEdits}, nil)
	data, err := os.ReadFile(filepath.Join(w.Root(), "new/sub/file.txt"))
	if result.IsError || err != nil || string(data) != "created" {
		t.Fatalf("nested create: %s, %v", result.Text(), err)
	}
}

func TestWorkspaceRecursiveSearchDoesNotReadEscapingLinks(t *testing.T) {
	w, parent := testWorkspace(t)
	putFile(t, filepath.Join(w.Root(), "inside.txt"), "MATCH_INSIDE")
	putFile(t, filepath.Join(parent, "secret.txt"), "MATCH_SECRET")
	makeLink(t, filepath.Join(parent, "secret.txt"), filepath.Join(w.Root(), "escape.txt"))
	makeLink(t, parent, filepath.Join(w.Root(), "escape-dir"))
	result := workspaceCall(t, w, "grep", map[string]any{"path": ".", "pattern": "MATCH"}, &permission.Policy{}, nil)
	if result.IsError || !strings.Contains(result.Text(), "MATCH_INSIDE") || strings.Contains(result.Text(), "MATCH_SECRET") {
		t.Fatalf("result=%s", result.Text())
	}
	result = workspaceCall(t, w, "ls", map[string]any{"path": "."}, &permission.Policy{}, nil)
	if !strings.Contains(result.Text(), "l escape.txt") {
		t.Fatalf("symlink listing=%s", result.Text())
	}

	approved := filepath.Join(parent, "approved")
	os.Mkdir(approved, 0700)
	putFile(t, filepath.Join(approved, "visible.txt"), "MATCH_APPROVED")
	makeLink(t, filepath.Join(parent, "secret.txt"), filepath.Join(approved, "escape.txt"))
	policy := &permission.Policy{Approver: func(context.Context, permission.Request) permission.Decision { return permission.Allowed() }}
	result = workspaceCall(t, w, "grep", map[string]any{"path": approved, "pattern": "MATCH"}, policy, nil)
	if !strings.Contains(result.Text(), "MATCH_APPROVED") || strings.Contains(result.Text(), "MATCH_SECRET") {
		t.Fatalf("directory grant escaped: %s", result.Text())
	}
}

func TestWorkspaceBindsApprovalToResolvedTarget(t *testing.T) {
	w, parent := testWorkspace(t)
	first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
	putFile(t, first, "APPROVED_CONTENT")
	putFile(t, second, "UNAPPROVED_SECRET")
	link := filepath.Join(w.Root(), "link")
	makeLink(t, first, link)
	policy := &permission.Policy{Approver: func(context.Context, permission.Request) permission.Decision {
		os.Remove(link)
		makeLink(t, second, link)
		return permission.Allowed()
	}}
	result := workspaceCall(t, w, "read", map[string]any{"path": "link"}, policy, nil)
	if !strings.Contains(result.Text(), "APPROVED_CONTENT") || strings.Contains(result.Text(), "UNAPPROVED_SECRET") {
		t.Fatalf("approval target changed: %s", result.Text())
	}

	policy.Approver = func(context.Context, permission.Request) permission.Decision {
		os.Remove(first)
		makeLink(t, second, first)
		return permission.Allowed()
	}
	result = workspaceCall(t, w, "read", map[string]any{"path": first}, policy, nil)
	if strings.Contains(result.Text(), "UNAPPROVED_SECRET") {
		t.Fatal("approved path replaced by symlink escaped grant")
	}
}

func TestWorkspaceRejectsDirectorySwapAfterPermission(t *testing.T) {
	for _, name := range []string{"read", "write"} {
		t.Run(name, func(t *testing.T) {
			w, parent := testWorkspace(t)
			dir := filepath.Join(w.Root(), "sub")
			os.Mkdir(dir, 0700)
			putFile(t, filepath.Join(dir, "target"), "inside")
			putFile(t, filepath.Join(parent, "target"), "UNAPPROVED_SECRET")
			result := workspaceCall(t, w, name, map[string]any{"path": "sub/target", "content": "changed"}, &permission.Policy{Mode: permission.ModeAcceptEdits}, func() {
				if err := os.Rename(dir, dir+"-old"); err != nil {
					t.Fatal(err)
				}
				makeLink(t, parent, dir)
			})
			data, _ := os.ReadFile(filepath.Join(parent, "target"))
			if !result.IsError || string(data) != "UNAPPROVED_SECRET" || strings.Contains(result.Text(), "UNAPPROVED_SECRET") {
				t.Fatalf("swap escaped: %s", result.Text())
			}
		})
	}
}

func TestWorkspaceFileApprovalCannotBecomeDirectoryGrant(t *testing.T) {
	w, parent := testWorkspace(t)
	path := filepath.Join(parent, "file")
	putFile(t, path, "original")
	policy := &permission.Policy{Approver: func(context.Context, permission.Request) permission.Decision {
		os.Remove(path)
		os.Mkdir(path, 0700)
		putFile(t, filepath.Join(path, "UNAPPROVED_SECRET"), "secret")
		return permission.Allowed()
	}}
	result := workspaceCall(t, w, "glob", map[string]any{"path": path, "pattern": "*"}, policy, nil)
	if !result.IsError || strings.Contains(result.Text(), "UNAPPROVED_SECRET") {
		t.Fatalf("file grant expanded: %s", result.Text())
	}
}

func TestWorkspaceRejectsSpecialAndHardLinkedWrites(t *testing.T) {
	w, parent := testWorkspace(t)
	putFile(t, filepath.Join(parent, "target"), "outside")
	if err := os.Link(filepath.Join(parent, "target"), filepath.Join(w.Root(), "hardlink")); err != nil {
		t.Fatal(err)
	}
	result := workspaceCall(t, w, "write", map[string]any{"path": "hardlink", "content": "changed"}, &permission.Policy{Mode: permission.ModeAcceptEdits}, nil)
	data, _ := os.ReadFile(filepath.Join(parent, "target"))
	if string(data) != "outside" || !strings.Contains(result.Text(), "hard-linked") {
		t.Fatalf("hardlink write: %s", result.Text())
	}
	fifo := filepath.Join(w.Root(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	result = workspaceCall(t, w, "read", map[string]any{"path": "fifo"}, &permission.Policy{}, nil)
	if !strings.Contains(result.Text(), "special file") {
		t.Fatalf("FIFO read: %s", result.Text())
	}
}

func TestWorkspaceDirectCallsAndShellDirectory(t *testing.T) {
	w, parent := testWorkspace(t)
	putFile(t, filepath.Join(parent, "outside"), "SECRET")
	for _, tool := range w.ReadOnlyTools() {
		_, err := tool.Execute(context.Background(), "direct", jargs(map[string]any{"path": parent, "pattern": "*"}), nil)
		if err == nil {
			t.Fatalf("direct %s bypassed approval", tool.Name())
		}
	}
	for _, tool := range w.CodingTools() {
		if tool.Name() != "bash" {
			continue
		}
		result, err := tool.Execute(context.Background(), "direct", jargs(map[string]any{"command": "pwd"}), nil)
		if err != nil || strings.TrimSpace(result.Content[0].(*llm.TextContent).Text) != w.Root() {
			t.Fatalf("shell dir: %+v, %v", result, err)
		}
	}
}

func TestWorkspaceInvalidRootsAndLinks(t *testing.T) {
	w, parent := testWorkspace(t)
	putFile(t, filepath.Join(parent, "file"), "x")
	for _, path := range []string{filepath.Join(parent, "missing"), filepath.Join(parent, "file")} {
		if invalid, err := tools.NewWorkspace(path); err == nil {
			invalid.Close()
			t.Fatalf("accepted invalid root %q", path)
		}
	}
	makeLink(t, "missing", filepath.Join(w.Root(), "dangling"))
	makeLink(t, "loop", filepath.Join(w.Root(), "loop"))
	for _, path := range []string{"dangling", "loop"} {
		result := workspaceCall(t, w, "read", map[string]any{"path": path}, &permission.Policy{Mode: permission.ModeBypass}, nil)
		if !result.IsError {
			t.Fatalf("accepted link %s: %s", path, result.Text())
		}
	}
}
