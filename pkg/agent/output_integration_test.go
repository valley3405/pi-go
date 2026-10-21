package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/valley3405/pi-go/pkg/agent"
	"github.com/valley3405/pi-go/pkg/llm"
	"github.com/valley3405/pi-go/pkg/tools"
)

type longOutputTool struct{ text string }

func (t *longOutputTool) Name() string { return "long-output" }
func (t *longOutputTool) Schema() llm.Tool {
	return llm.Tool{Name: "long-output", Description: "return long output", Parameters: map[string]any{"type": "object"}}
}
func (t *longOutputTool) ExecutionMode() agent.ExecutionMode { return agent.ExecutionParallel }
func (t *longOutputTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	return agent.ToolResult{Content: []llm.Content{&llm.TextContent{Text: t.text}}}, nil
}

func TestAgentCapturesCustomToolOutput(t *testing.T) {
	full := "0123456789界🙂"
	tool := &longOutputTool{text: full}
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "c1", Name: "long-output", Arguments: json.RawMessage(`{}`)}),
		llm.FauxAssistant("done"),
	}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	dir := t.TempDir()
	a := agent.New(agent.Options{
		Model:              model,
		Tools:              []agent.AgentTool{tool},
		StreamFn:           reg.StreamSimple,
		ToolOutputDir:      dir,
		MaxToolOutputChars: 4,
	})
	if err := a.Prompt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	var resultText string
	for _, msg := range a.Messages() {
		if msg.Role == llm.RoleToolResult {
			resultText = msg.Text()
		}
	}
	if !strings.Contains(resultText, "full output saved to") {
		t.Fatalf("custom tool output was not captured: %q", resultText)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("spill files=%v err=%v", files, err)
	}
	content, err := os.ReadFile(dir + string(os.PathSeparator) + files[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != full {
		t.Fatalf("spill content=%q, want %q", content, full)
	}
}

func TestAgentPreservesFullBashOutput(t *testing.T) {
	const outputLength = 105000
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "bash-large", Name: "bash", Arguments: json.RawMessage(`{"command":"printf 'x%.0s' {1..105000}"}`)}),
		llm.FauxAssistant("done"),
	}, 4)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	dir := t.TempDir()
	a := agent.New(agent.Options{
		Model:              model,
		Tools:              []agent.AgentTool{tools.Bash()},
		StreamFn:           reg.StreamSimple,
		ToolOutputDir:      dir,
		MaxToolOutputChars: 128,
	})
	if err := a.Prompt(context.Background(), "produce output"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("spill files=%v err=%v", files, err)
	}
	content, err := os.ReadFile(dir + string(os.PathSeparator) + files[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != outputLength {
		t.Fatalf("full bash output length=%d, want %d", len(content), outputLength)
	}
}
