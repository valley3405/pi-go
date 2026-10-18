package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/llm"
	"goagent/pkg/permission"
	"goagent/pkg/tools"
)

func jargs(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hello.txt")

	w := tools.Write()
	res, err := w.Execute(context.Background(), "c1", jargs(map[string]any{
		"path": path, "content": "line1\nline2\nline3\n",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].(*llm.TextContent).Text, "wrote") {
		t.Fatalf("unexpected write result: %v", res.Content[0])
	}

	r := tools.Read()
	res, err = r.Execute(context.Background(), "c2", jargs(map[string]any{"path": path}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "1\tline1") || !strings.Contains(out, "3\tline3") {
		t.Fatalf("unexpected read output:\n%s", out)
	}
}

func TestEditUniqueness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("foo bar foo"), 0o644)

	e := tools.Edit()
	// 多次匹配应报错
	res, _ := e.Execute(context.Background(), "c1", jargs(map[string]any{
		"path": path, "old_string": "foo", "new_string": "baz",
	}), nil)
	if !strings.Contains(res.Content[0].(*llm.TextContent).Text, "matches 2 times") {
		t.Fatalf("expected uniqueness error: %v", res.Content[0])
	}

	// replace_all 生效
	res, _ = e.Execute(context.Background(), "c2", jargs(map[string]any{
		"path": path, "old_string": "foo", "new_string": "baz", "replace_all": true,
	}), nil)
	data, _ := os.ReadFile(path)
	if string(data) != "baz bar baz" {
		t.Fatalf("replace_all failed: %q", data)
	}

	// 不存在应报错
	res, _ = e.Execute(context.Background(), "c3", jargs(map[string]any{
		"path": path, "old_string": "nope", "new_string": "x",
	}), nil)
	if !strings.Contains(res.Content[0].(*llm.TextContent).Text, "not found") {
		t.Fatalf("expected not-found error: %v", res.Content[0])
	}
}

func TestBash(t *testing.T) {
	b := tools.Bash()
	res, err := b.Execute(context.Background(), "c1", jargs(map[string]any{"command": "echo hello && exit 3"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "hello") || !strings.Contains(out, "exit code 3") {
		t.Fatalf("unexpected bash output: %q", out)
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Foo() {}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package b\nfunc Bar() { Foo() }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("Foo in text\n"), 0o644)

	g := tools.Grep()
	res, err := g.Execute(context.Background(), "c1", jargs(map[string]any{
		"pattern": "Foo", "path": dir, "include": "*.go",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "a.go:2") || !strings.Contains(out, "b.go:2") {
		t.Fatalf("grep missed matches:\n%s", out)
	}
	if strings.Contains(out, "c.txt") {
		t.Fatalf("include glob failed:\n%s", out)
	}
}

func TestLS(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "d1"), 0o755)
	os.WriteFile(filepath.Join(dir, "f1"), []byte("x"), 0o644)
	l := tools.LS()
	res, err := l.Execute(context.Background(), "c1", jargs(map[string]any{"path": dir}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "d d1/") || !strings.Contains(out, "f f1") {
		t.Fatalf("unexpected ls output:\n%s", out)
	}
}

func TestCodingToolsPermissionMetadata(t *testing.T) {
	readOnly := map[string]bool{"read": true, "ls": true, "grep": true}
	for _, tool := range tools.CodingTools() {
		readOnlyTool, ok := tool.(interface {
			IsReadOnly(json.RawMessage) bool
		})
		if !ok {
			t.Errorf("%s does not expose read-only metadata", tool.Name())
			continue
		}
		checkedTool, ok := tool.(interface {
			CheckPermissions(context.Context, json.RawMessage) permission.Decision
		})
		if !ok {
			t.Errorf("%s does not expose permission metadata", tool.Name())
			continue
		}
		expected := readOnly[tool.Name()]
		if actual := readOnlyTool.IsReadOnly(json.RawMessage(`{}`)); actual != expected {
			t.Errorf("%s readOnly=%v, want %v", tool.Name(), actual, expected)
		}
		decision := checkedTool.CheckPermissions(context.Background(), json.RawMessage(`{}`))
		expectedBehavior := permission.Ask
		if expected {
			expectedBehavior = permission.Allow
		}
		if decision.Behavior != expectedBehavior {
			t.Errorf("%s permission behavior=%q, want %q", tool.Name(), decision.Behavior, expectedBehavior)
		}
	}
}
