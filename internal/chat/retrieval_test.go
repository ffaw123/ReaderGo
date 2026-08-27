package chat

import (
	"math"
	"testing"

	"readergo/internal/model"
)

func TestFuseRetrievalChannelsSupportsIndependentAndSharedHits(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "dense-only", StartMS: 0},
		{ID: "bm25-only", StartMS: 1},
		{ID: "shared", StartMS: 2},
	}
	dense := []semanticHit{{SegmentID: "dense-only"}, {SegmentID: "shared"}}
	bm25 := []bm25WindowHit{{SegmentID: "bm25-only", Rank: 1}, {SegmentID: "shared", Rank: 2}}
	result := fuseRetrievalChannels(segments, dense, bm25)
	if len(result) != 3 || result[0].Segment.ID != "shared" {
		t.Fatalf("result = %#v, want shared hit first and both single-channel fallbacks", result)
	}
	if result[0].DenseRank != 2 || result[0].BM25Rank != 2 {
		t.Fatalf("shared ranks = %#v", result[0])
	}
}

func TestFuseRetrievalChannelsDeduplicatesWindows(t *testing.T) {
	segments := []model.VideoSegment{{ID: "same"}}
	result := fuseRetrievalChannels(segments,
		[]semanticHit{{SegmentID: "same"}, {SegmentID: "same"}},
		[]bm25WindowHit{{SegmentID: "same", Rank: 1}, {SegmentID: "same", Rank: 2}},
	)
	if len(result) != 1 || result[0].DenseRank != 1 || result[0].BM25Rank != 1 {
		t.Fatalf("deduplicated result = %#v", result)
	}
}

func TestFuseRetrievalChannelsTruncatesEachInputToEightAndFinalToFive(t *testing.T) {
	segments := make([]model.VideoSegment, 18)
	dense := make([]semanticHit, 9)
	bm25 := make([]bm25WindowHit, 9)
	for index := range segments {
		segments[index] = model.VideoSegment{ID: string(rune('a' + index)), StartMS: int64(index)}
		if index < 9 {
			dense[index] = semanticHit{SegmentID: segments[index].ID}
			bm25[index] = bm25WindowHit{SegmentID: segments[index+9].ID, Rank: index + 1}
		}
	}
	result := fuseRetrievalChannels(segments, dense, bm25)
	if len(result) != retrievalFinalTopK {
		t.Fatalf("final count = %d, want %d", len(result), retrievalFinalTopK)
	}
	for _, candidate := range result {
		if candidate.Segment.ID == "i" || candidate.Segment.ID == "r" {
			t.Fatalf("ninth channel candidate leaked into result: %#v", result)
		}
	}
}

func TestFuseRetrievalChannelsUsesFixedWeightedRRF(t *testing.T) {
	segments := []model.VideoSegment{{ID: "dense", StartMS: 0}, {ID: "bm25", StartMS: 1}, {ID: "both", StartMS: 2}}
	result := fuseRetrievalChannels(segments,
		[]semanticHit{{SegmentID: "dense"}, {SegmentID: "both"}},
		[]bm25WindowHit{{SegmentID: "bm25", Rank: 1}, {SegmentID: "both", Rank: 2}},
	)
	if result[0].Segment.ID != "both" || result[1].Segment.ID != "dense" || result[2].Segment.ID != "bm25" {
		t.Fatalf("weighted RRF order = %#v", result)
	}
	want := 1.0/12.0 + 0.25/12.0
	if math.Abs(result[0].RRFScore-want) > 1e-12 {
		t.Fatalf("shared score = %.15f, want %.15f", result[0].RRFScore, want)
	}
	if math.Abs(result[1].RRFScore-1.0/11.0) > 1e-12 || math.Abs(result[2].RRFScore-0.25/11.0) > 1e-12 {
		t.Fatalf("single-channel contributions = %#v", result)
	}
}

func TestFuseRetrievalChannelsBreaksTiesByStartTime(t *testing.T) {
	segments := []model.VideoSegment{{ID: "later", StartMS: 20}, {ID: "earlier", StartMS: 10}}
	// 构造同一排名的 BM25 命中，验证融合总分相同时只按窗口时间稳定排序。
	result := fuseRetrievalChannels(segments, nil, []bm25WindowHit{{SegmentID: "later", Rank: 1}, {SegmentID: "earlier", Rank: 1}})
	if len(result) != 2 || result[0].Segment.ID != "earlier" {
		t.Fatalf("tie order = %#v", result)
	}
}

func TestFuseRetrievalChannelsAllowsEmptyChannels(t *testing.T) {
	segments := []model.VideoSegment{{ID: "one"}}
	if got := fuseRetrievalChannels(segments, nil, nil); len(got) != 0 {
		t.Fatalf("empty channels = %#v", got)
	}
	if got := fuseRetrievalChannels(segments, []semanticHit{{SegmentID: "one"}}, nil); len(got) != 1 {
		t.Fatalf("dense fallback = %#v", got)
	}
	if got := fuseRetrievalChannels(segments, nil, []bm25WindowHit{{SegmentID: "one", Rank: 1}}); len(got) != 1 {
		t.Fatalf("BM25 fallback = %#v", got)
	}
}
