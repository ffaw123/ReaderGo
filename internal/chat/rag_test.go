package chat

import "testing"

func TestDecodeRAGAnswerValidatesCandidateIDs(t *testing.T) {
	windows := []ragWindow{{
		SegmentID: "allowed", StartMS: 120000, EndMS: 180000,
		Evidence: []ragEvidence{{SourceType: "asr", SourceID: "source", Text: "原始证据"}},
	}}
	output, err := decodeRAGAnswer(`{"answer":"视频在这一段解释了原因。","used_segment_ids":["allowed","allowed"]}`, windows)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.UsedSegmentIDs) != 1 || output.UsedSegmentIDs[0] != "allowed" {
		t.Fatalf("deduplicated IDs = %#v", output.UsedSegmentIDs)
	}
	if _, err := decodeRAGAnswer(`{"answer":"回答","used_segment_ids":["invented"]}`, windows); err == nil {
		t.Fatal("expected non-candidate citation to be rejected")
	}
	withoutEvidence := append(windows, ragWindow{SegmentID: "neighbor", StartMS: 180000, EndMS: 240000})
	if _, err := decodeRAGAnswer(`{"answer":"回答","used_segment_ids":["neighbor"]}`, withoutEvidence); err == nil {
		t.Fatal("expected citation without ASR/OCR evidence to be rejected")
	}
	if _, err := decodeRAGAnswer(`{"answer":"这是一个没有证据的结论。","used_segment_ids":[]}`, windows); err == nil {
		t.Fatal("expected an unsupported answer without citations to be rejected")
	}
}

func TestFormatTimestamp(t *testing.T) {
	if got := formatTimestamp(760000); got != "12:40" {
		t.Fatalf("timestamp = %q, want 12:40", got)
	}
	if got := formatTimestamp(3_661_000); got != "1:01:01" {
		t.Fatalf("timestamp = %q, want 1:01:01", got)
	}
}
