package chat

import "testing"

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
