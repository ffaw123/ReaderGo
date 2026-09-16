package visionagent

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestValidateRange(t *testing.T) {
	tests := []struct {
		value    Range
		duration int64
		want     error
	}{
		{Range{StartMS: 0, EndMS: 30_000}, 30_000, nil},
		{Range{StartMS: 0, EndMS: 1_500}, 30_000, ErrRangeTooShort},
		{Range{StartMS: 0, EndMS: 1_501}, 30_000, nil},
		{Range{StartMS: -1, EndMS: 1_000}, 30_000, ErrRangeInvalid},
		{Range{StartMS: 0, EndMS: 30_001}, 60_000, ErrRangeTooLong},
		{Range{StartMS: 20_000, EndMS: 30_000}, 29_999, ErrRangeOutOfBounds},
	}
	for _, test := range tests {
		if err := ValidateRange(test.value, test.duration); !errors.Is(err, test.want) {
			t.Fatalf("ValidateRange(%#v) = %v, want %v", test.value, err, test.want)
		}
	}
}

func TestFrameTimestampsAdjustIntervalToAtLeastFourFrames(t *testing.T) {
	got, err := frameTimestamps(InspectVideoRangeInput{StartMS: 13_000, EndMS: 20_000, IntervalMS: 5_000})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{13_000, 15_333, 17_666, 19_999}
	if !slices.Equal(got, want) {
		t.Fatalf("timestamps = %v, want %v", got, want)
	}
}

func TestFrameTimestampsAdjustIntervalToFrameBudget(t *testing.T) {
	got, err := frameTimestamps(InspectVideoRangeInput{StartMS: 0, EndMS: 30_000, IntervalMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxUniqueFrames {
		t.Fatalf("frame count = %d, want %d", len(got), MaxUniqueFrames)
	}
}

func TestFrameTimestampsUseHalfOpenRange(t *testing.T) {
	got, err := frameTimestamps(InspectVideoRangeInput{StartMS: 0, EndMS: 30_000, IntervalMS: 5_000})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{0, 5_000, 10_000, 15_000, 20_000, 25_000}
	if len(got) != len(want) {
		t.Fatalf("timestamps = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("timestamps = %v, want %v", got, want)
		}
	}
}

func TestDecodeObservationAcceptsPlainText(t *testing.T) {
	observation, err := decodeObservation("  人物举起杯子。  ")
	if err != nil {
		t.Fatal(err)
	}
	if observation.Observation != "人物举起杯子。" {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestDecodeFinalDecisionRejectsForgedObservationID(t *testing.T) {
	observations := map[string]ToolResult{
		"obs-1": {OK: true, Observation: "人物出现"},
	}
	_, err := decodeFinalDecision(`{"action":"FINAL","evidence_status":"GROUNDED","answer":"回答","used_observation_ids":["obs-2"]}`, observations)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}

func TestDecodeFinalDecisionRejectsGroundedWithoutObservation(t *testing.T) {
	_, err := decodeFinalDecision(`{"action":"FINAL","evidence_status":"GROUNDED","answer":"回答","used_observation_ids":[]}`, map[string]ToolResult{})
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}

func TestDecodeFinalDecisionRejectsEmptyObservation(t *testing.T) {
	observations := map[string]ToolResult{"obs-1": {OK: true}}
	_, err := decodeFinalDecision(`{"action":"FINAL","evidence_status":"GROUNDED","answer":"回答","used_observation_ids":["obs-1"]}`, observations)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}

func TestDecodeObservationRejectsStructuredOrInvalidText(t *testing.T) {
	values := []string{
		`{"observation":"人物出现"}`,
		"```text\n人物出现\n```",
		"",
		strings.Repeat("观察", MaxObservationRunes),
	}
	for _, value := range values {
		if _, err := decodeObservation(value); err == nil {
			t.Fatalf("accepted invalid observer output: %s", value)
		}
	}
}

func TestSafeObserverErrorClassifiesValidationFailures(t *testing.T) {
	tests := []struct {
		err  error
		code string
	}{{ErrInvalidOutput, "VISION_INVALID_OUTPUT"}, {ErrOutputTruncated, "VISION_OUTPUT_TRUNCATED"}}
	for _, test := range tests {
		if code, _ := safeObserverError(test.err); code != test.code {
			t.Fatalf("safeObserverError(%v) = %s, want %s", test.err, code, test.code)
		}
	}
}
