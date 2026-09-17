package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"readergo/internal/model"
)

func TestBuildRAGWindowsUsesVideoSegmentEvidence(t *testing.T) {
	result := retrievalResult{Candidates: []segmentCandidate{{
		Segment: model.VideoSegment{
			ID: "segment-1", StartMS: 0, EndMS: 60000,
			Transcript: "ASR 原文", OCRText: "OCR 原文",
		},
		Primary: true,
	}}}
	windows := buildRAGWindows(result)
	if len(windows) != 1 || len(windows[0].Evidence) != 2 {
		t.Fatalf("windows = %#v, want one window with ASR and OCR evidence", windows)
	}
	if windows[0].Evidence[0].SourceID != "segment-1:asr" || windows[0].Evidence[0].Text != "ASR 原文" {
		t.Fatalf("ASR evidence = %#v", windows[0].Evidence[0])
	}
	if windows[0].Evidence[1].SourceID != "segment-1:ocr" || windows[0].Evidence[1].Text != "OCR 原文" {
		t.Fatalf("OCR evidence = %#v", windows[0].Evidence[1])
	}
}

func TestRAGWindowJSONContainsOnlyActiveRetrievalRanks(t *testing.T) {
	windows := buildRAGWindows(retrievalResult{Candidates: []segmentCandidate{{
		Segment: model.VideoSegment{ID: "segment-1", Transcript: "证据"},
		Primary: true, DenseRank: 1, BM25Rank: 2,
	}}})
	payload, err := json.Marshal(windows)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	if strings.Contains(encoded, "exact_rank") || !strings.Contains(encoded, "dense_rank") || !strings.Contains(encoded, "bm25_rank") {
		t.Fatalf("candidate JSON contains stale or missing ranks: %s", encoded)
	}
}

func TestBuildRAGWindowsOmitsWindowsAfterEvidenceBudgetIsExhausted(t *testing.T) {
	candidateCount := ragMaxEvidenceRunes/ragMaxWindowEvidence + 1
	result := retrievalResult{Candidates: make([]segmentCandidate, 0, candidateCount)}
	for index := 0; index < candidateCount; index++ {
		result.Candidates = append(result.Candidates, segmentCandidate{Segment: model.VideoSegment{
			ID:         fmt.Sprintf("segment-%d", index),
			Transcript: strings.Repeat("证", ragMaxWindowEvidence),
		}})
	}

	windows := buildRAGWindows(result)
	want := ragMaxEvidenceRunes / ragMaxWindowEvidence
	if len(windows) != want {
		t.Fatalf("window count = %d, want %d evidence-bearing windows", len(windows), want)
	}
	for _, window := range windows {
		if len(window.Evidence) == 0 {
			t.Fatalf("empty-evidence window was retained: %#v", window)
		}
	}
}

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

func TestOverviewRAGCombinesSummaryAndFallsBackWithoutWindows(t *testing.T) {
	requests := make(chan []map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request.Messages
		answer := `{"answer":"摘要介绍了主题，片段说明了细节。","used_segment_ids":["segment-1"]}`
		if len(request.Messages) > 0 && request.Messages[0]["content"] == summaryAnswerSystemPrompt {
			answer = "摘要介绍了主题。"
		}
		encoded, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\ndata: [DONE]\n\n", encoded)
	}))
	defer server.Close()
	client, err := newStreamingTextClient("CHAT", server.URL+"/v1", "key", "model", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{answerClient: client}
	summary := &Capabilities{VideoTitle: "标题", Summary: SummaryContext{Overview: "总体摘要", ChapterTitles: []string{"章节"}}}
	decision := IntentDecision{Intent: IntentVideoOverview, Confidence: 0.8, RewrittenQuery: "完整问题"}
	windows := []ragWindow{{SegmentID: "segment-1", Evidence: []ragEvidence{{SourceType: "asr", Text: "片段原文"}}}}

	reply, err := service.answerRAGWindows(context.Background(), nil, "问题", decision, summary, windows, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := <-requests
	var payload map[string]any
	if err := json.Unmarshal([]byte(messages[1]["content"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if messages[0]["content"] != hybridAnswerSystemPrompt || payload["rewritten_query"] != "完整问题" || !strings.Contains(messages[1]["content"].(string), "总体摘要") || !strings.Contains(messages[1]["content"].(string), "片段原文") {
		t.Fatalf("unexpected hybrid input: %#v", messages)
	}
	if reply.Route.Intent != IntentVideoOverview || reply.Route.Mode != ModeRAG || len(reply.Citations) != 1 {
		t.Fatalf("unexpected hybrid reply: %#v", reply)
	}

	reply, err = service.answerRAGWindows(context.Background(), nil, "问题", decision, summary, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages = <-requests
	if messages[0]["content"] != summaryAnswerSystemPrompt || reply.Route.Mode != ModeSummary || !strings.Contains(reply.Route.Notice, "仅基于视频摘要") {
		t.Fatalf("unexpected summary fallback: %#v, %#v", messages, reply)
	}
	if _, err := decodeRAGAnswerWithSummary(`{"answer":"视频摘要介绍了主题。","used_segment_ids":[]}`, windows, true); err != nil {
		t.Fatalf("summary-only answer should be allowed in hybrid mode: %v", err)
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
