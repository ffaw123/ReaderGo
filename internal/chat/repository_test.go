package chat

import (
	"encoding/json"
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
	request := ReplyInput{Content: "问题", Vision: VisionRequest{Enabled: true, Range: &VisionRange{StartMS: 1_000, EndMS: 2_000}}}
	existing := model.Message{
		Content: persistedUserContent(request),
		Metadata: model.NewJSONDocument(map[string]any{
			"vision":                  VisionRequest{Enabled: true, Range: &VisionRange{StartMS: 1_000, EndMS: 2_000}},
			"requested_vision":        request.Vision,
			"vision_range_in_content": true,
		}),
	}
	if !sameReplyRequest(existing, request) {
		t.Fatal("相同的画面请求快照未被识别为幂等重试")
	}
	mismatch := request
	mismatch.Vision.Range = &VisionRange{StartMS: 1_000, EndMS: 3_000}
	if sameReplyRequest(existing, mismatch) {
		t.Fatal("不同时间区间被错误识别为同一幂等请求")
	}
	legacy := existing
	legacy.Content = request.Content
	legacy.Metadata = model.NewJSONDocument(map[string]any{"vision": request.Vision})
	if !sameReplyRequest(legacy, request) {
		t.Fatal("旧版未附时间范围的消息应继续支持幂等重试")
	}
	forged := request
	forged.Content = existing.Content
	if sameReplyRequest(existing, forged) {
		t.Fatal("带服务端范围标签的落库正文不能冒充原始用户问题")
	}
}

func TestUserMessageWithEffectiveVisionRangePreservesRequestedSnapshot(t *testing.T) {
	request := ReplyInput{
		Content: "这个人在做什么？",
		Vision:  VisionRequest{Enabled: true, Range: &VisionRange{StartMS: 1_000, EndMS: 2_000}},
	}
	requestedVision := cloneVisionRequest(request.Vision)
	user := model.Message{
		Content: persistedUserContent(request),
		Metadata: model.NewJSONDocument(userMessageMetadata{
			Vision:               cloneVisionRequest(request.Vision),
			RequestedVision:      &requestedVision,
			VisionRangeInContent: true,
		}),
	}

	updated, err := userMessageWithEffectiveVisionRange(user, &VisionRange{StartMS: 3_000, EndMS: 5_000})
	if err != nil {
		t.Fatalf("update effective vision range: %v", err)
	}
	wantContent := "这个人在做什么？\n\n[画面理解时间范围：00:03-00:05]"
	if updated.Content != wantContent {
		t.Fatalf("updated content = %q, want %q", updated.Content, wantContent)
	}
	var metadata userMessageMetadata
	if err := json.Unmarshal(updated.Metadata, &metadata); err != nil {
		t.Fatalf("decode updated metadata: %v", err)
	}
	if metadata.Vision.Range == nil || *metadata.Vision.Range != (VisionRange{StartMS: 3_000, EndMS: 5_000}) {
		t.Fatalf("effective vision range = %#v", metadata.Vision.Range)
	}
	if metadata.RequestedVision == nil || metadata.RequestedVision.Range == nil ||
		*metadata.RequestedVision.Range != (VisionRange{StartMS: 1_000, EndMS: 2_000}) {
		t.Fatalf("requested vision snapshot = %#v", metadata.RequestedVision)
	}
	if !sameReplyRequest(updated, request) {
		t.Fatal("更新最终范围后，相同的初始请求应继续命中幂等校验")
	}
	effectiveRetry := request
	effectiveRetry.Vision.Range = &VisionRange{StartMS: 3_000, EndMS: 5_000}
	if sameReplyRequest(updated, effectiveRetry) {
		t.Fatal("最终范围不能替代初始范围参与幂等校验")
	}
}

func TestPersistedUserContentIncludesVisionRange(t *testing.T) {
	request := ReplyInput{
		Content: "这个人在做什么？",
		Vision:  VisionRequest{Enabled: true, Range: &VisionRange{StartMS: 61_000, EndMS: 72_000}},
	}
	want := "这个人在做什么？\n\n[画面理解时间范围：01:01-01:12]"
	if got := persistedUserContent(request); got != want {
		t.Fatalf("persisted user content = %q, want %q", got, want)
	}
	request.Vision.Enabled = false
	if got := persistedUserContent(request); got != request.Content {
		t.Fatalf("non-vision content = %q, want original content", got)
	}
}
