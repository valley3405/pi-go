package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"goagent/pkg/llm"
)

func TestCaptureToolResultLeavesSmallOutput(t *testing.T) {
	result := ToolResult{Content: []llm.Content{&llm.TextContent{Text: "small"}}}
	captured := captureToolResult(result, "test", "c1", outputConfig{maxChars: 10})
	if got := captured.Content[0].(*llm.TextContent).Text; got != "small" {
		t.Fatalf("small output changed: %q", got)
	}
}

func TestCaptureToolResultTruncatesUnicode(t *testing.T) {
	result := ToolResult{Content: []llm.Content{&llm.TextContent{Text: "0123界456🙂89"}}}
	captured := captureToolResult(result, "test", "c1", outputConfig{maxChars: 8})
	text := captured.Content[0].(*llm.TextContent).Text
	if !strings.HasPrefix(text, "0123") || !strings.HasSuffix(text, "6🙂89") || !strings.Contains(text, "chars truncated") {
		t.Fatalf("unexpected Unicode truncation: %q", text)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("truncation produced invalid UTF-8: %q", text)
	}
}

func TestCaptureToolResultSpillsFullOutput(t *testing.T) {
	dir := t.TempDir()
	image := &llm.ImageContent{Data: "aW1hZ2U=", MimeType: "image/png"}
	result := ToolResult{Content: []llm.Content{
		&llm.TextContent{Text: "abc"},
		image,
		&llm.TextContent{Text: "def界🙂"},
	}}
	captured := captureToolResult(result, "test", "c/1", outputConfig{dir: dir, maxChars: 4})
	if len(captured.Content) != 2 || captured.Content[1] != image {
		t.Fatalf("non-text block was not preserved: %#v", captured.Content)
	}
	text := captured.Content[0].(*llm.TextContent).Text
	if !strings.Contains(text, "full output saved to") {
		t.Fatalf("missing spill path: %q", text)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("spill files=%v err=%v", files, err)
	}
	full, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != "abcdef界🙂" {
		t.Fatalf("full output=%q", full)
	}
}

func TestCaptureToolResultSpillFailureFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := ToolResult{Content: []llm.Content{&llm.TextContent{Text: "0123456789"}}}
	captured := captureToolResult(result, "test", "c1", outputConfig{dir: path, maxChars: 4})
	text := captured.Content[0].(*llm.TextContent).Text
	if !strings.Contains(text, "chars truncated") || !strings.Contains(text, "could not be saved") {
		t.Fatalf("missing fallback diagnostics: %q", text)
	}
}
