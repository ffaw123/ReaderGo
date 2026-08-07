package chat

import (
	"testing"

	"readergo/internal/model"
)

func TestBuildSegmentTextDocumentsUsesVideoSegmentCopies(t *testing.T) {
	documents := buildSegmentTextDocuments([]model.VideoSegment{
		{ID: "both", Transcript: " ASR 原文 ", OCRText: " OCR 原文 "},
		{ID: "empty"},
	})
	if len(documents) != 2 {
		t.Fatalf("got %d documents, want ASR and OCR documents", len(documents))
	}
	if documents[0].SegmentID != "both" || documents[0].SourceType != "asr" || documents[0].Text != "ASR 原文" {
		t.Fatalf("ASR document = %#v", documents[0])
	}
	if documents[1].SegmentID != "both" || documents[1].SourceType != "ocr" || documents[1].Text != "OCR 原文" {
		t.Fatalf("OCR document = %#v", documents[1])
	}
}

func TestFuseRetrievalChannelsUsesThreeIndependentRanks(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "a", StartMS: 0, EndMS: 60000},
		{ID: "b", StartMS: 60000, EndMS: 120000},
	}
	dense := []semanticHit{{SegmentID: "b", Similarity: .9}}
	exact := []rankedSegmentText{
		{Document: segmentTextDocument{SegmentID: "a", SourceType: "asr"}, Score: 5},
		{Document: segmentTextDocument{SegmentID: "b", SourceType: "ocr"}, Score: 5},
	}
	bm25 := []rankedSegmentText{{Document: segmentTextDocument{SegmentID: "a", SourceType: "asr"}, Score: 3}}

	result := fuseRetrievalChannels(segments, dense, exact, bm25)
	if len(result) != 2 {
		t.Fatalf("got %d candidates, want 2", len(result))
	}
	if result[0].Segment.ID != "a" || result[0].ExactRank != 1 || result[0].BM25Rank != 1 {
		t.Fatalf("top candidate = %#v, want segment a with exact and BM25 ranks", result[0])
	}
	if result[1].Segment.ID != "b" || result[1].DenseRank != 1 || result[1].ExactRank != 1 {
		t.Fatalf("second candidate = %#v, want segment b with dense and exact ranks", result[1])
	}
}

func TestSegmentTextMapsDirectlyToOwningWindow(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "left", StartMS: 0, EndMS: 60000},
		{ID: "right", StartMS: 60000, EndMS: 120000},
	}
	exact := []rankedSegmentText{{Document: segmentTextDocument{SegmentID: "right", SourceType: "asr"}, Score: 1}}
	result := fuseRetrievalChannels(segments, nil, exact, nil)
	if len(result) != 1 || result[0].Segment.ID != "right" {
		t.Fatalf("segment mapping = %#v, want only right window", result)
	}
}

func TestSegmentRankingUsesHigherASROrOCRScore(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "crowded", StartMS: 0, EndMS: 60000},
		{ID: "later", StartMS: 60000, EndMS: 120000},
	}
	hits := []rankedSegmentText{
		{Document: segmentTextDocument{SegmentID: "crowded", SourceType: "asr"}, Score: 90},
		{Document: segmentTextDocument{SegmentID: "crowded", SourceType: "ocr"}, Score: 100},
		{Document: segmentTextDocument{SegmentID: "later", SourceType: "asr"}, Score: 10},
	}
	ranks := segmentWindowRanks(segments, hits, 8)
	if ranks["crowded"] != 1 || ranks["later"] != 2 {
		t.Fatalf("window ranks = %#v, want the best ASR/OCR score per window", ranks)
	}
}
