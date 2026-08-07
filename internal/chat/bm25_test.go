package chat

import (
	"slices"
	"testing"
)

func TestLexicalTokensPreserveIdentifiersAndSplitChinese(t *testing.T) {
	tokens := lexicalTokens("二叉树前序遍历使用 GPT-4.1 和 RAG_BASE_URL")
	for _, expected := range []string{"二叉", "叉树", "前序", "遍历", "gpt-4.1", "rag_base_url"} {
		if !slices.Contains(tokens, expected) {
			t.Fatalf("tokens %v do not contain %q", tokens, expected)
		}
	}
}

func TestBM25AndExactRankSegmentTexts(t *testing.T) {
	documents := []segmentTextDocument{
		{SegmentID: "segment-1", SourceType: "asr", Text: "这一节介绍二叉树的前序遍历，模型版本是 GPT-4.1。"},
		{SegmentID: "segment-2", SourceType: "ocr", Text: "数据库连接配置与部署说明"},
	}
	bm25 := rankBM25("二叉树前序遍历", documents, 8)
	if len(bm25) == 0 || bm25[0].Document.SegmentID != "segment-1" {
		t.Fatalf("BM25 top result = %#v, want segment-1", bm25)
	}
	exact := rankExact("视频中的 GPT-4.1 是什么", documents, 8)
	if len(exact) == 0 || exact[0].Document.SegmentID != "segment-1" {
		t.Fatalf("exact top result = %#v, want segment-1", exact)
	}
}
