package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"readergo/internal/model"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

func TestNewRAGIndexProcessorAcceptsOneToTenConcurrentRequests(t *testing.T) {
	for _, concurrency := range []int{1, 5, 10} {
		if _, err := NewRAGIndexProcessor(&gorm.DB{}, "https://example.com/v1", "test-key", "chat-model", concurrency, time.Minute); err != nil {
			t.Errorf("concurrency %d was rejected: %v", concurrency, err)
		}
	}
	for _, concurrency := range []int{0, 11} {
		if _, err := NewRAGIndexProcessor(&gorm.DB{}, "https://example.com/v1", "test-key", "chat-model", concurrency, time.Minute); err == nil {
			t.Errorf("concurrency %d was accepted", concurrency)
		}
	}
}

func TestBuildRAGWindowsUsesSixtySecondBoundariesAndDeduplicatesOCR(t *testing.T) {
	windows, err := buildRAGWindows(context.Background(), 125000, []model.ASRSegment{
		{StartMS: 0, EndMS: 60000, Transcript: " 第一段\n内容 "},
		{StartMS: 60000, EndMS: 120000, Transcript: "第二段"},
	}, []model.OCRSegment{
		{StartMS: 0, EndMS: 15000, Text: "固定 标题"},
		{StartMS: 15000, EndMS: 30000, Text: "固定  标题"},
		{StartMS: 120000, EndMS: 125000, Text: "尾页"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(windows))
	}
	if windows[0].StartMS != 0 || windows[0].EndMS != 60000 || windows[0].Transcript != "第一段\n内容" {
		t.Errorf("first window = %#v", windows[0])
	}
	if windows[0].OCRText != "固定 标题" || strings.Count(windows[0].Evidence, "固定 标题") != 1 {
		t.Errorf("first OCR evidence was not deduplicated: %#v", windows[0])
	}
	if windows[2].StartMS != 120000 || windows[2].EndMS != 125000 || windows[2].OCRText != "尾页" {
		t.Errorf("tail window = %#v", windows[2])
	}
	if len(windows[0].SourceHash) != 64 || windows[0].ID == "" {
		t.Errorf("window identifiers were not generated: %#v", windows[0])
	}
}

func TestRAGCompatibleClientGeneratesStructuredSegmentsAndEmbeddings(t *testing.T) {
	windows := []*ragWindow{
		{ID: "segment-1", StartMS: 0, EndMS: 60000, Evidence: "[0,60000) ASR: 原始证据一"},
		{ID: "segment-2", StartMS: 60000, EndMS: 120000, Evidence: "[60000,120000) OCR: 原始证据二"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			request, input := decodeRAGChatRequest(t, r)
			if request.Model != "rag-chat" || request.ResponseFormat.Type != "json_object" {
				t.Errorf("chat request = %#v", request)
			}
			if len(request.Messages) != 2 || input.SegmentID == "" {
				t.Errorf("chat messages = %#v", request.Messages)
			}
			output := ragSegmentOutput{SegmentID: input.SegmentID, SegmentSummary: "第二段摘要", Keywords: []string{"OCR"}}
			if input.SegmentID == "segment-1" {
				output = ragSegmentOutput{SegmentID: input.SegmentID, SegmentSummary: "第一段摘要", KeyFacts: []string{"事实A", "事实A"}, Keywords: []string{"主题"}}
			}
			writeRAGChatResponse(t, w, output)
		case "/v1/embeddings":
			var request struct {
				Model      string   `json:"model"`
				Input      []string `json:"input"`
				Dimensions int      `json:"dimensions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Model != "rag-embedding" || request.Dimensions != ragEmbeddingDimensions || len(request.Input) != 2 || strings.Contains(request.Input[0], "原始证据") {
				t.Errorf("embedding request = %#v", request)
			}
			vector := make([]float64, ragEmbeddingDimensions)
			data := []map[string]any{
				{"object": "embedding", "embedding": vector, "index": 1},
				{"object": "embedding", "embedding": vector, "index": 0},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "rag-embedding", "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	processor := &RAGIndexProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL+"/v1"), option.WithMaxRetries(0)),
		chatModel: "rag-chat", concurrency: 2,
	}
	if err := processor.summarizeWindows(context.Background(), windows); err != nil {
		t.Fatal(err)
	}
	if windows[0].SegmentSummary != "第一段摘要" || len(windows[0].KeyFacts) != 1 {
		t.Errorf("first generated window = %#v", windows[0])
	}
	if !strings.Contains(windows[0].RetrievalText, "事实A") || strings.Contains(windows[0].RetrievalText, "原始证据") {
		t.Errorf("retrieval text = %q", windows[0].RetrievalText)
	}
	embedding := &EmbeddingProcessor{
		client:         openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL+"/v1"), option.WithMaxRetries(0)),
		embeddingModel: "rag-embedding",
	}
	segments := []model.VideoSegment{{RetrievalText: windows[0].RetrievalText}, {RetrievalText: windows[1].RetrievalText}}
	vectors, err := embedding.embedSegments(context.Background(), segments)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || len(vectors[0]) != ragEmbeddingDimensions || len(vectors[1]) != ragEmbeddingDimensions {
		t.Errorf("vector dimensions = %d/%d", len(vectors[0]), len(vectors[1]))
	}
}

func TestSummarizeWindowsLimitsConcurrencyToTen(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		_, input := decodeRAGChatRequest(t, r)
		time.Sleep(40 * time.Millisecond)
		writeRAGChatResponse(t, w, ragSegmentOutput{SegmentID: input.SegmentID, SegmentSummary: "摘要"})
	}))
	defer server.Close()

	windows := make([]*ragWindow, 12)
	for index := range windows {
		windows[index] = &ragWindow{ID: fmt.Sprintf("segment-%d", index), Evidence: "证据"}
	}
	processor := &RAGIndexProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		chatModel: "rag-chat", concurrency: 10,
	}
	if err := processor.summarizeWindows(context.Background(), windows); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 10 {
		t.Errorf("maximum concurrent requests = %d, want 10", maximum.Load())
	}
}

func TestSummarizeWindowUsesSDKToRetryTransientFailureThreeTimes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, input := decodeRAGChatRequest(t, r)
		if calls.Add(1) <= ragMaxRetries {
			w.Header().Set("Retry-After-Ms", "1")
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		writeRAGChatResponse(t, w, ragSegmentOutput{SegmentID: input.SegmentID, SegmentSummary: "成功摘要"})
	}))
	defer server.Close()

	processor := &RAGIndexProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		chatModel: "rag-chat", concurrency: 1,
	}
	if err := processor.summarizeWindows(context.Background(), []*ragWindow{{ID: "segment-retry", Evidence: "证据"}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != ragMaxRetries+1 {
		t.Errorf("calls = %d, want %d", calls.Load(), ragMaxRetries+1)
	}
}

func TestSummarizeWindowRetriesInvalidStructuredOutputTwice(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, input := decodeRAGChatRequest(t, r)
		if calls.Add(1) <= ragMaxOutputRetries {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-rag","object":"chat.completion","created":0,"model":"rag-chat","choices":[{"index":0,"message":{"role":"assistant","content":"not-json"},"finish_reason":"stop"}]}`)
			return
		}
		writeRAGChatResponse(t, w, ragSegmentOutput{SegmentID: input.SegmentID, SegmentSummary: "成功摘要"})
	}))
	defer server.Close()

	processor := &RAGIndexProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		chatModel: "rag-chat", concurrency: 1,
	}
	window := &ragWindow{ID: "segment-json", Evidence: "证据"}
	if err := processor.summarizeWindows(context.Background(), []*ragWindow{window}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != ragMaxOutputRetries+1 {
		t.Errorf("calls = %d, want %d", calls.Load(), ragMaxOutputRetries+1)
	}
	if window.SegmentSummary != "成功摘要" {
		t.Errorf("summary = %q, want 成功摘要", window.SegmentSummary)
	}
}

func TestSummarizeWindowDoesNotRetryPermanentFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	}))
	defer server.Close()

	processor := &RAGIndexProcessor{
		client:    openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		chatModel: "rag-chat", concurrency: 1,
	}
	err := processor.summarizeWindows(context.Background(), []*ragWindow{{ID: "segment-auth", Evidence: "证据"}})
	if err == nil {
		t.Fatal("permanent API failure was accepted")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if action := decideFailureAction(err, 1, 4); action != failureActionFailWithDeadLetter {
		t.Errorf("401 failure action = %d, want immediate failure with DLQ", action)
	}
}

func TestHasUsableRAGEvidenceRequiresNonWhitespaceText(t *testing.T) {
	if hasUsableRAGEvidence([]model.ASRSegment{{Transcript: " \n\t "}}, []model.OCRSegment{{Text: "  "}}) {
		t.Fatal("whitespace-only evidence was accepted")
	}
	if !hasUsableRAGEvidence([]model.ASRSegment{{Transcript: "有效语音"}}, nil) {
		t.Fatal("non-empty ASR evidence was rejected")
	}
	if !hasUsableRAGEvidence(nil, []model.OCRSegment{{Text: "画面文字"}}) {
		t.Fatal("non-empty OCR evidence was rejected")
	}
}

func TestEmbedWindowsUsesTenItemBatchesAndRunsAllBatchesConcurrently(t *testing.T) {
	var active, maximum, calls, fullBatches, tailBatches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRAGEmbeddingRequest(t, r)
		calls.Add(1)
		switch len(request.Input) {
		case ragEmbeddingBatchSize:
			fullBatches.Add(1)
		case 5:
			tailBatches.Add(1)
		default:
			t.Errorf("embedding batch size = %d, want 10 or 5", len(request.Input))
		}

		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		time.Sleep(40 * time.Millisecond)

		indices := make([]int, len(request.Input))
		for index := range indices {
			indices[index] = len(indices) - 1 - index
		}
		writeRAGEmbeddingResponse(t, w, request.Input, indices, ragEmbeddingDimensions)
	}))
	defer server.Close()

	segments := make([]model.VideoSegment, 25)
	for index := range segments {
		segments[index] = model.VideoSegment{RetrievalText: fmt.Sprintf("segment-%d", index)}
	}
	processor := &EmbeddingProcessor{
		client:         openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		embeddingModel: "rag-embedding",
	}
	vectors, err := processor.embedSegments(context.Background(), segments)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || fullBatches.Load() != 2 || tailBatches.Load() != 1 {
		t.Errorf("calls/full/tail = %d/%d/%d, want 3/2/1", calls.Load(), fullBatches.Load(), tailBatches.Load())
	}
	if maximum.Load() != 3 {
		t.Errorf("maximum concurrent embedding requests = %d, want 3", maximum.Load())
	}
	for index, vector := range vectors {
		if len(vector) != ragEmbeddingDimensions || vector[0] != float64(index) {
			t.Fatalf("vector %d is not aligned with its input", index)
		}
	}
}

func TestEmbedBatchRetriesMalformedAndInvalidResponsesTwice(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRAGEmbeddingRequest(t, r)
		attempt := calls.Add(1)
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":`)
			return
		}
		if attempt == 2 {
			writeRAGEmbeddingResponse(t, w, request.Input, []int{0, 0, 0}, ragEmbeddingDimensions)
			return
		}
		writeRAGEmbeddingResponse(t, w, request.Input, []int{2, 1, 0}, ragEmbeddingDimensions)
	}))
	defer server.Close()

	processor := &EmbeddingProcessor{
		client:         openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		embeddingModel: "rag-embedding",
	}
	segments := []model.VideoSegment{{RetrievalText: "segment-0"}, {RetrievalText: "segment-1"}, {RetrievalText: "segment-2"}}
	vectors, err := processor.embedSegments(context.Background(), segments)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != ragMaxOutputRetries+1 {
		t.Errorf("calls = %d, want %d", calls.Load(), ragMaxOutputRetries+1)
	}
	for index, vector := range vectors {
		if vector[0] != float64(index) {
			t.Errorf("vector %d is not aligned with its input", index)
		}
	}
}

func TestEmbedBatchDoesNotRetryDimensionMismatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeRAGEmbeddingRequest(t, r)
		calls.Add(1)
		writeRAGEmbeddingResponse(t, w, request.Input, []int{0}, ragEmbeddingDimensions-1)
	}))
	defer server.Close()

	processor := &EmbeddingProcessor{
		client:         openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL), option.WithMaxRetries(0)),
		embeddingModel: "rag-embedding",
	}
	_, err := processor.embedSegments(context.Background(), []model.VideoSegment{{RetrievalText: "segment-0"}})
	if err == nil {
		t.Fatal("embedding dimension mismatch was accepted")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if action := decideFailureAction(err, 1, 4); action != failureActionFailWithDeadLetter {
		t.Errorf("dimension mismatch failure action = %d, want immediate failure with DLQ", action)
	}
}

func TestRouterRegistersRAGIndexProcessor(t *testing.T) {
	rag := &RAGIndexProcessor{}
	embedding := &EmbeddingProcessor{}
	summary := &SummaryProcessor{}
	router := NewRouterProcessor(nil, nil, nil, rag, embedding, summary)
	if router.processors[model.TaskRAGIndex] != rag {
		t.Fatal("RAG index processor was not registered")
	}
	if router.processors[model.TaskEmbedding] != embedding || router.processors[model.TaskSummary] != summary {
		t.Fatal("embedding or summary processor was not registered")
	}
}

func TestFormatPGVectorRejectsNonFiniteValues(t *testing.T) {
	vector := make([]float64, ragEmbeddingDimensions)
	vector[7] = math.NaN()
	if _, err := formatPGVector(vector); err == nil {
		t.Fatal("formatPGVector accepted NaN")
	}
}

type ragChatTestRequest struct {
	Model          string `json:"model"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type ragEmbeddingTestRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
}

func decodeRAGEmbeddingRequest(t *testing.T, r *http.Request) ragEmbeddingTestRequest {
	t.Helper()
	var request ragEmbeddingTestRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode embedding request: %v", err)
	}
	if request.Model != "rag-embedding" || request.Dimensions != ragEmbeddingDimensions {
		t.Errorf("embedding request = %#v", request)
	}
	return request
}

func writeRAGEmbeddingResponse(t *testing.T, w http.ResponseWriter, inputs []string, indices []int, dimensions int) {
	t.Helper()
	data := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		vector := make([]float64, dimensions)
		if index >= 0 && index < len(inputs) && dimensions > 0 {
			var globalIndex int
			if _, err := fmt.Sscanf(inputs[index], "segment-%d", &globalIndex); err == nil {
				vector[0] = float64(globalIndex)
			}
		}
		data = append(data, map[string]any{"object": "embedding", "embedding": vector, "index": index})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "data": data, "model": "rag-embedding",
		"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
	}); err != nil {
		t.Errorf("encode embedding response: %v", err)
	}
}

func decodeRAGChatRequest(t *testing.T, r *http.Request) (ragChatTestRequest, ragSegmentInput) {
	t.Helper()
	var request ragChatTestRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode chat request: %v", err)
		return request, ragSegmentInput{}
	}
	if len(request.Messages) != 2 {
		t.Errorf("chat message count = %d, want 2", len(request.Messages))
		return request, ragSegmentInput{}
	}
	var input ragSegmentInput
	if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
		t.Errorf("decode chat input: %v", err)
	}
	return request, input
}

func writeRAGChatResponse(t *testing.T, w http.ResponseWriter, output ragSegmentOutput) {
	t.Helper()
	content, err := json.Marshal(output)
	if err != nil {
		t.Errorf("encode chat output: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-rag", "object": "chat.completion", "created": 0, "model": "rag-chat",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}},
	}); err != nil {
		t.Errorf("encode chat response: %v", err)
	}
}
