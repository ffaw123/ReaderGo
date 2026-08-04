package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"readergo/internal/media"
	"readergo/internal/model"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

func TestDeduplicateFramesStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&OCRProcessor{}).deduplicateFrames(ctx, []media.FrameSample{{Path: "must-not-be-read.jpg"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("deduplicateFrames() error = %v, want context.Canceled", err)
	}
}

func TestDeduplicateFramesAlwaysMergesSameTimestamp(t *testing.T) {
	processor := &OCRProcessor{hashDistance: 0}
	frames, err := processor.deduplicateFrames(context.Background(), []media.FrameSample{
		{TimestampMS: 15000, Path: createOCRTestImage(t)},
		// 第二个路径故意不存在：同毫秒采样应在读取图片前直接合并。
		{TimestampMS: 15000, Path: "does-not-exist.jpg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].TimestampMS != 15000 {
		t.Fatalf("frames = %#v, want one frame at 15000 ms", frames)
	}
}

func TestBuildOCRSegmentsDropsOnlyEmptyResults(t *testing.T) {
	video := model.Video{ID: "video-id", TenantID: "tenant-id"}
	frames := []ocrFrame{{TimestampMS: 0}, {TimestampMS: 15000}, {TimestampMS: 30000}}
	results := []string{"  ", "模型返回的任意文字", "  原样文字!?  "}
	segments := buildOCRSegments(video, 45000, frames, results)
	if len(segments) != 2 {
		t.Fatalf("segments = %#v, want two non-empty segments", segments)
	}
	if segments[0].StartMS != 15000 || segments[0].EndMS != 30000 || segments[0].Text != "模型返回的任意文字" {
		t.Errorf("first segment = %#v", segments[0])
	}
	if segments[1].StartMS != 30000 || segments[1].EndMS != 45000 || segments[1].Text != "原样文字!?" {
		t.Errorf("second segment = %#v", segments[1])
	}
}

func TestOCRRecognizeImageUsesQwenVLOCRContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer ocr-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL    string `json:"url"`
						Detail string `json:"detail"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
			ResponseFormat any `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "qwen-vl-ocr" || len(request.Messages) != 1 || request.Messages[0].Role != "user" || request.ResponseFormat != nil {
			t.Errorf("unexpected OCR request: %#v", request)
		}
		if len(request.Messages[0].Content) != 2 || request.Messages[0].Content[0].Type != "image_url" || request.Messages[0].Content[0].ImageURL.Detail != "" {
			t.Errorf("unexpected image content: %#v", request.Messages[0].Content)
		}
		if !strings.HasPrefix(request.Messages[0].Content[0].ImageURL.URL, "data:image/jpeg;base64,") {
			t.Errorf("image URL is not a JPEG data URL")
		}
		if request.Messages[0].Content[1].Type != "text" || request.Messages[0].Content[1].Text != ocrUserPrompt {
			t.Errorf("unexpected OCR prompt: %#v", request.Messages[0].Content[1])
		}
		writeOCRCompletion(t, w, "原样文字!?")
	}))
	defer server.Close()

	processor := newTestOCRProcessor(server, "qwen-vl-ocr", 1, 0)
	result, err := processor.recognizeImage(context.Background(), createOCRTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	if result != "原样文字!?" {
		t.Errorf("result = %q", result)
	}
}

func TestRecognizeFramesUsesConfiguredConcurrency(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		time.Sleep(50 * time.Millisecond)
		writeOCRCompletion(t, w, "")
	}))
	defer server.Close()

	imagePath := createOCRTestImage(t)
	processor := newTestOCRProcessor(server, "ocr-model", 2, 0)
	frames := []ocrFrame{{Path: imagePath}, {Path: imagePath}, {Path: imagePath}, {Path: imagePath}}
	results, err := processor.recognizeFrames(context.Background(), frames)
	if err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Errorf("maximum concurrent requests = %d, want 2", maximum.Load())
	}
	if len(results) != len(frames) {
		t.Fatalf("result count = %d, want %d", len(results), len(frames))
	}
}

func TestOCRRecognizeRetriesTemporaryHTTPFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		writeOCRCompletion(t, w, "重试成功")
	}))
	defer server.Close()

	processor := newTestOCRProcessor(server, "ocr-model", 1, 2)
	result, err := processor.recognizeImage(context.Background(), createOCRTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || result != "重试成功" {
		t.Errorf("calls = %d, result = %q", calls.Load(), result)
	}
}

func TestNewOCRProcessorAcceptsOneToTenConcurrentRequests(t *testing.T) {
	for _, concurrency := range []int{1, 10} {
		if _, err := NewOCRProcessor(&gorm.DB{}, new(media.FFmpeg), "localhost:9000", "key", "secret", false, "videos", "https://example.com/v1", "ocr-key", "qwen-vl-ocr", 0.3, 5, concurrency); err != nil {
			t.Fatalf("concurrency %d: %v", concurrency, err)
		}
	}
	for _, concurrency := range []int{0, 11} {
		if _, err := NewOCRProcessor(&gorm.DB{}, new(media.FFmpeg), "localhost:9000", "key", "secret", false, "videos", "https://example.com/v1", "ocr-key", "qwen-vl-ocr", 0.3, 5, concurrency); err == nil {
			t.Fatalf("concurrency %d unexpectedly accepted", concurrency)
		}
	}
}

func TestRouterRegistersOCRForExistingVisionTask(t *testing.T) {
	ocr := &OCRProcessor{}
	router := NewRouterProcessor(nil, ocr, nil)
	if router.processors[model.TaskVision] != ocr {
		t.Fatal("OCR processor was not registered for vision task")
	}
}

func newTestOCRProcessor(server *httptest.Server, modelName string, workers, retries int) *OCRProcessor {
	client := openai.NewClient(
		option.WithAPIKey("ocr-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(retries),
	)
	return &OCRProcessor{client: client, model: modelName, workers: workers}
}

func writeOCRCompletion(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, err := fmt.Fprintf(w, `{"id":"ocr-test","object":"chat.completion","created":1,"model":"ocr-model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, content)
	if err != nil {
		t.Error(err)
	}
}

func createOCRTestImage(t *testing.T) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "ocr-*.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}
