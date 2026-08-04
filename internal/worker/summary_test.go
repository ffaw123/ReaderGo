package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"readergo/internal/model"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func TestDecodeAndValidateSummaryOutputRequiresContiguousCoverage(t *testing.T) {
	valid := summaryOutput{
		VideoID: "video-1", Overview: "总体概览",
		Chapters: []summaryChapter{
			{Title: "第一章", SourceUnits: []int{1, 2}, Content: "第一章内容", KeyPoints: []string{"要点一"}},
			{Title: "第二章", SourceUnits: []int{3}, Content: "第二章内容"},
		},
	}
	content, _ := json.Marshal(valid)
	output, err := decodeAndValidateSummaryOutput(string(content), "video-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Chapters) != 2 || output.Chapters[0].Title != "第一章" {
		t.Fatalf("output = %#v", output)
	}

	invalid := valid
	invalid.Chapters = append([]summaryChapter(nil), valid.Chapters...)
	invalid.Chapters[1].SourceUnits = []int{4}
	content, _ = json.Marshal(invalid)
	if _, err := decodeAndValidateSummaryOutput(string(content), "video-1", 3); err == nil {
		t.Fatal("summary with a missing source unit was accepted")
	}
}

func TestGenerateSummaryRetriesInvalidOutputInsideTask(t *testing.T) {
	oldDelays := summaryOutputRetryDelays
	summaryOutputRetryDelays = [...]time.Duration{0, 0}
	t.Cleanup(func() { summaryOutputRetryDelays = oldDelays })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= summaryMaxOutputRetries {
			writeSummaryChatResponse(t, w, "not-json")
			return
		}
		content, _ := json.Marshal(summaryOutput{
			VideoID: "video-1", Overview: "总体概览",
			Chapters: []summaryChapter{{Title: "完整章节", SourceUnits: []int{1, 2}, Content: "详细解释", KeyPoints: []string{"关键点"}}},
		})
		writeSummaryChatResponse(t, w, string(content))
	}))
	defer server.Close()

	processor := &SummaryProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		chatModel: "rag-chat",
	}
	output, err := processor.generateSummary(context.Background(), summaryInput{
		VideoID: "video-1", Units: []summarySourceUnit{{Unit: 1}, {Unit: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != summaryMaxOutputRetries+1 || len(output.Chapters) != 1 {
		t.Fatalf("calls/output = %d/%#v", calls.Load(), output)
	}
}

func TestBuildSummaryMarkdownOmitsInternalUnitsAndTimes(t *testing.T) {
	markdown := buildSummaryMarkdown("知识分享", summaryOutput{
		Overview: "这是总体概览。",
		Chapters: []summaryChapter{{
			Title: "核心原理", SourceUnits: []int{1, 2}, Content: "这里是详细解释。", KeyPoints: []string{"重要事实"},
		}},
	})
	for _, want := range []string{"# 知识分享", "## 总体概览", "### 1. 核心原理", "#### 本章要点", "- 重要事实"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown does not contain %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "source_units") || strings.Contains(markdown, "start_ms") {
		t.Errorf("markdown leaked internal boundaries:\n%s", markdown)
	}
}

func TestSummaryEvidenceRequiresRawTranscriptOrOCR(t *testing.T) {
	if hasUsableSummaryEvidence([]model.VideoSegment{{SegmentSummary: "只有代理摘要"}}) {
		t.Fatal("proxy-only segment was accepted as raw summary evidence")
	}
	if !hasUsableSummaryEvidence([]model.VideoSegment{{OCRText: "画面文字"}}) {
		t.Fatal("valid OCR evidence was rejected")
	}
}

func writeSummaryChatResponse(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"id":"chatcmpl-summary","object":"chat.completion","created":0,"model":"rag-chat","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, content); err != nil {
		t.Errorf("write summary response: %v", err)
	}
}
