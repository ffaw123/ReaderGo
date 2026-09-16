package visionagent

import "testing"

func TestTrimMemoriesEnforcesTurnLimit(t *testing.T) {
	values := make([]Memory, MaxMemoryTurns+2)
	for index := range values {
		values[index] = Memory{Status: StatusGrounded, Observation: "观察"}
	}
	if got := trimMemories(values); len(got) != MaxMemoryTurns {
		t.Fatalf("memory count = %d, want %d", len(got), MaxMemoryTurns)
	}
}
