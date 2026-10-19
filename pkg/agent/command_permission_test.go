package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
	"goagent/pkg/permission"
	"goagent/pkg/tools"
)

func runBashWithPolicy(t *testing.T, command string, policy *permission.Policy) []agent.AgentMessage {
	t.Helper()
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxToolCall("", &llm.ToolCallContent{ID: "bash-1", Name: "bash", Arguments: json.RawMessage(`{"command":` + strconvQuote(command) + `}`)}),
		llm.FauxAssistant("done"),
	}, 16)
	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")
	a := agent.New(agent.Options{
		Model:      model,
		Tools:      []agent.AgentTool{tools.Bash()},
		StreamFn:   reg.StreamSimple,
		Permission: policy,
	})
	if err := a.Prompt(context.Background(), "run command"); err != nil {
		t.Fatal(err)
	}
	waitForIdle(t, a)
	return a.Messages()
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestBashCommandAllowSkipsApproval(t *testing.T) {
	policy := &permission.Policy{AllowedCommands: []string{`printf allowed`}}
	messages := runBashWithPolicy(t, "printf allowed", policy)
	var result string
	for _, msg := range messages {
		if msg.Role == llm.RoleToolResult {
			result = msg.Text()
		}
	}
	if !strings.Contains(result, "allowed") {
		t.Fatalf("allowed command did not run: %#v", messages)
	}
}

func TestBashCommandDenyBeatsBypass(t *testing.T) {
	policy := &permission.Policy{Mode: permission.ModeBypass, DeniedCommands: []string{`forbidden`}}
	messages := runBashWithPolicy(t, "printf forbidden", policy)
	var result agent.AgentMessage
	for _, msg := range messages {
		if msg.Role == llm.RoleToolResult {
			result = msg
		}
	}
	if !result.IsError || !strings.Contains(result.Text(), "bash command is disallowed") {
		t.Fatalf("denied command result: %#v", result)
	}
}
