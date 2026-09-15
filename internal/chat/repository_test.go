package chat

import (
	"testing"

	"readergo/internal/model"
)

func TestParseLegacySummaryMarkdown(t *testing.T) {
	context := parseLegacySummaryMarkdown(`# 标题

## 总体概览

这是总体内容。

## 内容章节

### 1. 第一章

正文

### 2. 第二章
`)
	if context.Overview != "这是总体内容。" {
		t.Fatalf("overview = %q", context.Overview)
	}
	if len(context.ChapterTitles) != 2 || context.ChapterTitles[0] != "第一章" || context.ChapterTitles[1] != "第二章" {
		t.Fatalf("chapter titles = %#v", context.ChapterTitles)
	}
}

func TestSameReplyRequestIncludesVisionSnapshot(t *testing.T) {
	existing := model.Message{
		Content: "问题",
		Metadata: model.NewJSONDocument(map[string]any{"vision": VisionRequest{
			Enabled: true, Range: &VisionRange{StartMS: 1_000, EndMS: 2_000},
		}}),
	}
	matching := ReplyInput{Content: "问题", Vision: VisionRequest{Enabled: true, Range: &VisionRange{StartMS: 1_000, EndMS: 2_000}}}
	if !sameReplyRequest(existing, matching) {
		t.Fatal("相同的画面请求快照未被识别为幂等重试")
	}
	mismatch := matching
	mismatch.Vision.Range = &VisionRange{StartMS: 1_000, EndMS: 3_000}
	if sameReplyRequest(existing, mismatch) {
		t.Fatal("不同时间区间被错误识别为同一幂等请求")
	}
}
