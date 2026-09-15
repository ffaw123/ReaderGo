package visionagent

import (
	"errors"
	"testing"
)

func TestValidateRange(t *testing.T) {
	tests := []struct {
		name     string
		value    Range
		duration int64
		wantErr  error
	}{
		{name: "valid maximum", value: Range{StartMS: 0, EndMS: 30_000}, duration: 30_000},
		{name: "negative start", value: Range{StartMS: -1, EndMS: 1_000}, duration: 30_000, wantErr: ErrRangeInvalid},
		{name: "empty", value: Range{StartMS: 1_000, EndMS: 1_000}, duration: 30_000, wantErr: ErrRangeInvalid},
		{name: "too long", value: Range{StartMS: 0, EndMS: 30_001}, duration: 60_000, wantErr: ErrRangeTooLong},
		{name: "out of video", value: Range{StartMS: 20_000, EndMS: 30_000}, duration: 29_999, wantErr: ErrRangeOutOfBounds},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRange(test.value, test.duration)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ValidateRange() error = %v, want %v", err, test.wantErr)
			}
		})
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

func TestDecodeDecisionRejectsUnsampledCitation(t *testing.T) {
	sampled := map[int64]Frame{5_000: {TimestampMS: 5_000}}
	_, err := decodeDecision(`{"action":"FINAL","evidence_status":"GROUNDED","observation":"人物出现","used_frame_timestamps_ms":[6000]}`, sampled)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("decodeDecision() error = %v, want ErrInvalidOutput", err)
	}
}

func TestDecodeDecisionAcceptsToolCall(t *testing.T) {
	decision, err := decodeDecision(`{"action":"CALL_TOOL","reason":"需要细看","tool":{"name":"inspect_video_range","arguments":{"start_ms":1000,"end_ms":2000,"interval_ms":500}}}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Tool == nil || decision.Tool.Arguments.IntervalMS != 500 {
		t.Fatalf("unexpected decision: %#v", decision)
	}
}

func TestDecodeDecisionAcceptsGroundedAndNoEvidence(t *testing.T) {
	sampled := map[int64]Frame{5_000: {TimestampMS: 5_000}}
	grounded, err := decodeDecision(`{"action":"FINAL","evidence_status":"GROUNDED","observation":"人物出现","used_frame_timestamps_ms":[5000]}`, sampled)
	if err != nil || grounded.Observation != "人物出现" {
		t.Fatalf("grounded decision = %#v, error = %v", grounded, err)
	}
	noEvidence, err := decodeDecision(`{"action":"FINAL","evidence_status":"NO_EVIDENCE","observation":"","used_frame_timestamps_ms":[]}`, sampled)
	if err != nil || noEvidence.EvidenceStatus != "NO_EVIDENCE" {
		t.Fatalf("no-evidence decision = %#v, error = %v", noEvidence, err)
	}
}

func TestDecodeDecisionRejectsExtraAndTrailingContent(t *testing.T) {
	for _, content := range []string{
		`{"action":"FINAL","evidence_status":"NO_EVIDENCE","observation":"","used_frame_timestamps_ms":[],"extra":true}`,
		`{"action":"FINAL","evidence_status":"NO_EVIDENCE","observation":"","used_frame_timestamps_ms":[]} trailing`,
	} {
		if _, err := decodeDecision(content, nil); err == nil {
			t.Fatalf("decodeDecision(%q) accepted invalid output", content)
		}
	}
}
