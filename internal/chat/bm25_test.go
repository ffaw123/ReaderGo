package chat

import (
	"math"
	"slices"
	"strings"
	"testing"

	"readergo/internal/model"
)

func TestLexicalTokensPreserveIdentifiersAndSplitChinese(t *testing.T) {
	tokens := lexicalTokens("二叉树前序遍历使用 GPT-4.1 和 RAG_BASE_URL")
	for _, expected := range []string{"二叉", "叉树", "前序", "遍历", "gpt-4.1", "rag_base_url"} {
		if !slices.Contains(tokens, expected) {
			t.Fatalf("tokens %v do not contain %q", tokens, expected)
		}
	}
}

func TestFilterRawTextByQueryKeepsOnlyMatchingChunks(t *testing.T) {
	queryTokens := lexicalTokens("向量数据库")
	got := filterRawTextByQuery("开场闲聊。介绍向量数据库，继续无关内容；数据库索引优化\n结束", queryTokens)
	if strings.Contains(got, "开场") || strings.Contains(got, "无关") || strings.Contains(got, "结束") {
		t.Fatalf("filtered text retained irrelevant chunks: %q", got)
	}
	if !strings.Contains(got, "向量数据库") || !strings.Contains(got, "数据库索引") {
		t.Fatalf("filtered text lost matching chunks: %q", got)
	}
}

func TestRankBM25WindowsUsesAllConfiguredFields(t *testing.T) {
	tests := []struct {
		name    string
		segment model.VideoSegment
	}{
		{name: "retrieval_text", segment: model.VideoSegment{ID: "retrieval", RetrievalText: "target"}},
		{name: "transcript", segment: model.VideoSegment{ID: "transcript", Transcript: "noise. target sentence. ignored"}},
		{name: "ocr_text", segment: model.VideoSegment{ID: "ocr", OCRText: "noise；target label；ignored"}},
		{name: "all fields", segment: model.VideoSegment{ID: "all", RetrievalText: "target", Transcript: "target", OCRText: "target"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hits := rankBM25Windows("target", []model.VideoSegment{test.segment}, 8)
			if len(hits) != 1 || hits[0].SegmentID != test.segment.ID || hits[0].Rank != 1 {
				t.Fatalf("hits = %#v", hits)
			}
		})
	}
}

func TestRankBM25WindowsProducesOneCandidateForASRAndOCR(t *testing.T) {
	segments := []model.VideoSegment{{ID: "same-window", Transcript: "target", OCRText: "target"}}
	hits := rankBM25Windows("target", segments, 8)
	if len(hits) != 1 || hits[0].SegmentID != "same-window" {
		t.Fatalf("hits = %#v, want one window candidate", hits)
	}
}

func TestRankBM25WindowsHandlesEmptyAndStopTokenQueries(t *testing.T) {
	segments := []model.VideoSegment{{ID: "empty"}, {ID: "text", RetrievalText: "任意内容"}}
	for _, query := range []string{"", "什么", "   "} {
		if hits := rankBM25Windows(query, segments, 8); len(hits) != 0 {
			t.Fatalf("query %q hits = %#v, want none", query, hits)
		}
	}
	if hits := rankBM25Windows("target", nil, 8); len(hits) != 0 {
		t.Fatalf("nil documents hits = %#v", hits)
	}
}

func TestRankBM25WindowsAppliesFieldWeightsAndLengthNormalization(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "retrieval-short", StartMS: 0, RetrievalText: "target"},
		{ID: "retrieval-long", StartMS: 1, RetrievalText: "target " + strings.Repeat("noise ", 20)},
		{ID: "transcript", StartMS: 2, Transcript: "target"},
	}
	hits := rankBM25Windows("target", segments, 8)
	if len(hits) != 3 {
		t.Fatalf("hits = %#v", hits)
	}
	if hits[0].SegmentID != "retrieval-short" {
		t.Fatalf("top hit = %#v, want short retrieval_text field", hits[0])
	}
	if hits[2].SegmentID != "transcript" {
		t.Fatalf("last hit = %#v, want lower-weight transcript field", hits[2])
	}
}

func TestRankBM25WindowsCountsDocumentFrequencyOncePerWindow(t *testing.T) {
	segment := model.VideoSegment{ID: "all", RetrievalText: "target", Transcript: "target", OCRText: "target"}
	hits := rankBM25Windows("target", []model.VideoSegment{segment}, 8)
	if len(hits) != 1 {
		t.Fatalf("hits = %#v", hits)
	}
	weightedTF := 1.0 + 0.25 + 0.25
	idf := math.Log(1 + 0.5/1.5)
	want := idf * ((bm25K1 + 1) * weightedTF) / (bm25K1 + weightedTF)
	if math.Abs(hits[0].Score-want) > 1e-12 {
		t.Fatalf("score = %.15f, want %.15f; document frequency may have been counted per field", hits[0].Score, want)
	}
}

func TestRankBM25WindowsBreaksScoreTiesByStartTimeAndLimitsTopK(t *testing.T) {
	segments := make([]model.VideoSegment, 10)
	for index := range segments {
		segments[index] = model.VideoSegment{ID: string(rune('a' + index)), StartMS: int64(10-index) * 1000, RetrievalText: "target"}
	}
	hits := rankBM25Windows("target", segments, 8)
	if len(hits) != 8 {
		t.Fatalf("hit count = %d, want 8", len(hits))
	}
	if hits[0].SegmentID != "j" || hits[7].SegmentID != "c" {
		t.Fatalf("tie order = %#v", hits)
	}
}
