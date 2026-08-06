package chat

import "testing"

func TestPartialJSONAnswerDecodesOnlyAnswerContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		answer   string
		complete bool
	}{
		{name: "partial text", content: `{"answer":"第一段`, answer: "第一段"},
		{name: "escaped newline", content: `{"answer":"第一行\n第二行`, answer: "第一行\n第二行"},
		{name: "partial escape", content: `{"answer":"内容\u4e`, answer: "内容"},
		{name: "surrogate pair", content: `{"answer":"表情\ud83d\ude00","used_segment_ids":[]}`, answer: "表情😀", complete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, found, complete, err := partialJSONAnswer(test.content)
			if err != nil {
				t.Fatalf("partialJSONAnswer: %v", err)
			}
			if !found || answer != test.answer || complete != test.complete {
				t.Fatalf("got answer=%q found=%v complete=%v", answer, found, complete)
			}
		})
	}
}

func TestRAGAnswerStreamerDoesNotExposeJSONEnvelope(t *testing.T) {
	var streamed string
	streamer := ragAnswerStreamer{onDelta: func(delta string) error {
		streamed += delta
		return nil
	}}
	for _, delta := range []string{`{"ans`, `wer":"你好`, `\n世界","used_segment_ids":[]}`} {
		if err := streamer.Accept(delta); err != nil {
			t.Fatalf("accept: %v", err)
		}
	}
	if streamed != "你好\n世界" {
		t.Fatalf("streamed = %q", streamed)
	}
}

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
