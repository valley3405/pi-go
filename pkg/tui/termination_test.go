package tui

import (
	"strings"
	"testing"

	"goagent/pkg/agent"
)

func TestTerminationReasonIsVisible(t *testing.T) {
	m := New(newTestHarness(t, nil, false))
	m = applyAgentEvent(t, m, agent.Event{Type: agent.EventAgentEnd, Reason: agent.TerminationTimeout})
	if m.busy {
		t.Fatal("agent end left model busy")
	}
	if !strings.Contains(m.statusView(), "终止原因："+string(agent.TerminationTimeout)) {
		t.Fatalf("termination reason missing from status: %q", m.statusView())
	}
}
