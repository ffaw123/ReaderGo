package visionagent

import (
	"strings"
	"testing"
)

func TestBuildCombinedRetrievalTextKeepsBaseAndGroundedObservations(t *testing.T) {
	windows := []RetrievalWindow{{SegmentID: "segment-1", StartMS: 0, EndMS: 60_000, RetrievalText: "原始检索文本"}}
	memories := []Memory{
		{StartMS: 0, EndMS: 10_000, Status: StatusGrounded, Observation: "历史画面结果"},
		{StartMS: 10_000, EndMS: 20_000, Status: StatusNoEvidence},
	}
	combined := buildCombinedRetrievalText(windows, memories, Decision{EvidenceStatus: "GROUNDED", Observation: "本轮画面结果"})
	for _, expected := range []string{"原始检索文本", "历史画面结果", "本轮画面结果"} {
		if !strings.Contains(combined, expected) {
			t.Fatalf("combined text does not contain %q: %s", expected, combined)
		}
	}
	if strings.Contains(combined, StatusNoEvidence) {
		t.Fatalf("no-evidence memory entered combined text: %s", combined)
	}
}

func TestTrimMemoriesEnforcesTurnLimit(t *testing.T) {
	values := make([]Memory, MaxMemoryTurns+2)
	for index := range values {
		values[index] = Memory{Status: StatusGrounded, Observation: "观察"}
	}
	if got := trimMemories(values); len(got) != MaxMemoryTurns {
		t.Fatalf("memory count = %d, want %d", len(got), MaxMemoryTurns)
	}
}
