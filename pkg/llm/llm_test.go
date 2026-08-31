package llm_test

import (
	"context"
	"encoding/json"
	"testing"

	"goagent/pkg/llm"
)

func TestFauxStreamEvents(t *testing.T) {
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxAssistant("hello world"),
	}, 3)
	model := faux.Models()[0]

	s := faux.Stream(context.Background(), model, llm.Context{
		SystemPrompt: "sys",
		Messages:     []llm.Message{llm.NewUserMessage("hi")},
	}, llm.StreamOptions{})

	var types []llm.EventType
	var accumulated string
	for ev := range s.Events() {
		types = append(types, ev.Type)
		if ev.Type == llm.EventTextDelta {
			accumulated += ev.Delta
		}
	}
	if accumulated != "hello world" {
		t.Fatalf("deltas accumulated to %q", accumulated)
	}
	if types[0] != llm.EventStart || types[len(types)-1] != llm.EventDone {
		t.Fatalf("event sequence wrong: %v", types)
	}
	result, err := s.Result()
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != "hello world" || result.StopReason != llm.StopStop {
		t.Fatalf("bad result: %+v", result)
	}
}

func TestRegistry(t *testing.T) {
	reg := llm.NewRegistry()
	reg.SetProvider(llm.NewFauxProvider("faux", nil, 1))
	m, err := reg.GetModel("faux", "faux-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "faux" {
		t.Fatalf("model provider = %q", m.Provider)
	}
	if _, err := reg.GetModel("nope", "x"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestMessageJSONRoundTrip(t *testing.T) {
	orig := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.Content{
			&llm.TextContent{Text: "hi"},
			&llm.ThinkingContent{Thinking: "hmm"},
			&llm.ToolCallContent{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		},
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{Input: 10, Output: 5, TotalTokens: 15},
	}
	b, err := orig.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back llm.Message
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if len(back.Content) != 3 {
		t.Fatalf("content count = %d", len(back.Content))
	}
	tc := back.Content[2].(*llm.ToolCallContent)
	if tc.Name != "bash" || string(tc.Arguments) != `{"command":"ls"}` {
		t.Fatalf("toolcall round-trip failed: %+v", tc)
	}
	if back.Usage.TotalTokens != 15 || back.StopReason != llm.StopToolUse {
		t.Fatalf("metadata lost: %+v", back)
	}
}

func TestValidateToolArguments(t *testing.T) {
	tool := llm.Tool{
		Name: "t",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
	}
	if err := llm.ValidateToolArguments(tool, json.RawMessage(`{"path":"/tmp"}`)); err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateToolArguments(tool, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected missing-required error")
	}
	if err := llm.ValidateToolArguments(tool, json.RawMessage(`{"path":123}`)); err == nil {
		t.Fatal("expected type error")
	}
	if err := llm.ValidateToolArguments(tool, json.RawMessage(`{bad`)); err == nil {
		t.Fatal("expected JSON error")
	}
}
