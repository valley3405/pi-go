package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valley3405/pi-go/pkg/llm"
	"github.com/valley3405/pi-go/pkg/tools"
)

func TestGlobRecursiveAndRootPatterns(t *testing.T) {
	dir := t.TempDir()
	for path, content := range map[string]string{
		"a.go":                    "package a",
		"b.txt":                   "text",
		"sub/c.go":                "package c",
		"sub/deep/d.go":           "package d",
		".git/ignored.go":         "ignored",
		"node_modules/ignored.go": "ignored",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	glob := tools.Glob()
	res, err := glob.Execute(context.Background(), "c1", jargs(map[string]any{"pattern": "**/*.go", "path": dir}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/d.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("recursive glob missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"b.txt", ".git", "node_modules"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("recursive glob contains %q:\n%s", unwanted, out)
		}
	}

	res, err = glob.Execute(context.Background(), "c2", jargs(map[string]any{"pattern": "*.go", "path": dir}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out = res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "a.go") || strings.Contains(out, "sub/c.go") {
		t.Fatalf("root-only glob mismatch:\n%s", out)
	}
}

func TestGlobMaxResults(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := tools.Glob().Execute(context.Background(), "c1", jargs(map[string]any{
		"pattern": "*.go", "path": dir, "max_results": 1,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "a.go") || strings.Contains(out, "b.go") || !strings.Contains(out, "truncated at 1 matches") {
		t.Fatalf("unexpected limited glob output:\n%s", out)
	}
}
