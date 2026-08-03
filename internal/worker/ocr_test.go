package worker

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"readergo/internal/media"
	"readergo/internal/model"
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

func TestOCRRecognizeRetriesAndFiltersLowConfidenceText(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"errorCode":0,"errorMsg":"Success","result":{"ocrResults":[{"prunedResult":{"rec_texts":["有效文字","低置信度文字"],"rec_scores":[0.91,0.2]}}]}}`)
	}))
	defer server.Close()

	imagePath := createOCRTestImage(t)
	processor := &OCRProcessor{
		endpoint: server.URL, client: server.Client(), minConfidence: 0.6,
		retryDelays: []time.Duration{0, 0},
	}
	result, err := processor.recognizeWithRetry(context.Background(), imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if result.Text != "有效文字" || result.Confidence != 0.91 {
		t.Errorf("result = %#v", result)
	}
}

func TestRecognizeFramesUsesConfiguredConcurrency(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"errorCode":0,"result":{"ocrResults":[{"prunedResult":{"rec_texts":[],"rec_scores":[]}}]}}`)
	}))
	defer server.Close()

	imagePath := createOCRTestImage(t)
	processor := &OCRProcessor{
		endpoint: server.URL, client: server.Client(), minConfidence: 0.6, workers: 2,
		retryDelays: []time.Duration{0, 0},
	}
	frames := []ocrFrame{{Path: imagePath}, {Path: imagePath}, {Path: imagePath}, {Path: imagePath}}
	results, err := processor.recognizeFrames(context.Background(), frames)
	if err != nil {
		t.Fatal(err)
	}
	if maximum.Load() < 2 {
		t.Errorf("maximum concurrent requests = %d, want at least 2", maximum.Load())
	}
	for index, result := range results {
		if !result.Done {
			t.Errorf("result %d was not completed", index)
		}
	}
}

func TestOCRRecognizeReturnsServiceUnavailableAfterThreeAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	processor := &OCRProcessor{
		endpoint: server.URL, client: server.Client(), minConfidence: 0.6,
		retryDelays: []time.Duration{0, 0},
	}
	_, err := processor.recognizeWithRetry(context.Background(), createOCRTestImage(t))
	if _, ok := err.(*ocrServiceUnavailableError); !ok {
		t.Fatalf("error = %T %v, want *ocrServiceUnavailableError", err, err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestRouterRegistersOCRForExistingVisionTask(t *testing.T) {
	ocr := &OCRProcessor{}
	router := NewRouterProcessor(nil, ocr)
	if router.processors[model.TaskVision] != ocr {
		t.Fatal("OCR processor was not registered for vision task")
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
