package chat

import (
	"testing"

	"readergo/internal/model"
)

func TestFuseRetrievalChannelsUsesThreeIndependentRanks(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "a", StartMS: 0, EndMS: 60000},
		{ID: "b", StartMS: 60000, EndMS: 120000},
	}
	dense := []semanticHit{{SegmentID: "b", Similarity: .9}}
	exact := []rankedSource{{Document: sourceDocument{SourceID: "cross", StartMS: 59000, EndMS: 61000}, Score: 5}}
	bm25 := []rankedSource{{Document: sourceDocument{SourceID: "early", StartMS: 1000, EndMS: 2000}, Score: 3}}

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

func TestSourceAtBoundaryMapsOnlyToRightWindow(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "left", StartMS: 0, EndMS: 60000},
		{ID: "right", StartMS: 60000, EndMS: 120000},
	}
	exact := []rankedSource{{Document: sourceDocument{SourceID: "source", StartMS: 60000, EndMS: 61000}, Score: 1}}
	result := fuseRetrievalChannels(segments, nil, exact, nil)
	if len(result) != 1 || result[0].Segment.ID != "right" {
		t.Fatalf("boundary mapping = %#v, want only right window", result)
	}
}

func TestSourceRankingTakesTopWindowsAfterAggregation(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "crowded", StartMS: 0, EndMS: 60000},
		{ID: "later", StartMS: 60000, EndMS: 120000},
	}
	hits := make([]rankedSource, 0, 10)
	for index := 0; index < 9; index++ {
		hits = append(hits, rankedSource{Document: sourceDocument{SourceID: string(rune('a' + index)), StartMS: int64(index), EndMS: int64(index + 1)}, Score: float64(100 - index)})
	}
	hits = append(hits, rankedSource{Document: sourceDocument{SourceID: "later-source", StartMS: 61000, EndMS: 62000}, Score: 10})
	ranks := sourceWindowRanks(segments, hits, 8)
	if ranks["crowded"] != 1 || ranks["later"] != 2 {
		t.Fatalf("window ranks = %#v, want both windows after source aggregation", ranks)
	}
}
