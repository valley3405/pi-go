package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goagent/pkg/llm"
	"goagent/pkg/tools"
)

func TestMultiEditAppliesOperationsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("foo bar foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tools.MultiEdit().Execute(context.Background(), "c1", jargs(map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_string": "foo", "new_string": "baz", "replace_all": true},
			{"old_string": "baz bar", "new_string": "qux"},
		},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "qux baz\n" {
		t.Fatalf("updated content=%q", content)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "2 edit(s), 3 replacement(s)") {
		t.Fatalf("unexpected summary: %q", out)
	}
}

func TestMultiEditFailureDoesNotWritePartialChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.txt")
	original := "one two\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tools.MultiEdit().Execute(context.Background(), "c1", jargs(map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_string": "one", "new_string": "changed"},
			{"old_string": "missing", "new_string": "x"},
		},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Content[0].(*llm.TextContent).Text; !strings.Contains(out, "edit 2") {
		t.Fatalf("missing failing edit index: %q", out)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != original {
		t.Fatalf("partial changes were written: %q", content)
	}
}
