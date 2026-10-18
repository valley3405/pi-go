package main

import (
	"testing"

	"goagent/pkg/agent"
)

func TestTerminationExitCode(t *testing.T) {
	tests := []struct {
		reason agent.TerminationReason
		code   int
	}{
		{reason: "", code: 0},
		{reason: agent.TerminationCompleted, code: 0},
		{reason: agent.TerminationAborted, code: 130},
		{reason: agent.TerminationTimeout, code: 124},
		{reason: agent.TerminationMaxTurns, code: 1},
		{reason: agent.TerminationOutputLimit, code: 1},
		{reason: agent.TerminationModelError, code: 1},
		{reason: agent.TerminationPromptTooLong, code: 1},
		{reason: agent.TerminationError, code: 1},
	}
	for _, tt := range tests {
		if code := terminationExitCode(tt.reason); code != tt.code {
			t.Errorf("reason %q: code=%d, want %d", tt.reason, code, tt.code)
		}
	}
}
