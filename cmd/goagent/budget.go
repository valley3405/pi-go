package main

import "goagent/pkg/agent"

func terminationExitCode(reason agent.TerminationReason) int {
	switch reason {
	case "", agent.TerminationCompleted:
		return 0
	case agent.TerminationAborted:
		return 130
	case agent.TerminationTimeout:
		return 124
	default:
		return 1
	}
}
