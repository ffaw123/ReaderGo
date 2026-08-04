package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func TestRetryDelayUsesConfiguredAttemptBackoff(t *testing.T) {
	consumer := Consumer{options: Options{RetryDelays: []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute}}}
	if got := consumer.retryDelay(1); got != 30*time.Second {
		t.Fatalf("retryDelay(1) = %s, want 30s", got)
	}
	if got := consumer.retryDelay(3); got != 30*time.Minute {
		t.Fatalf("retryDelay(3) = %s, want 30m", got)
	}
}

func TestDecideFailureActionClassifiesModelAndUserInputErrors(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		currentAttempt int
		maxAttempts    int
		want           failureAction
	}{
		{name: "429 remains retryable", err: &openai.Error{StatusCode: http.StatusTooManyRequests}, currentAttempt: 1, maxAttempts: 4, want: failureActionRetry},
		{name: "5xx remains retryable", err: &openai.Error{StatusCode: http.StatusServiceUnavailable}, currentAttempt: 1, maxAttempts: 4, want: failureActionRetry},
		{name: "timeout remains retryable", err: context.DeadlineExceeded, currentAttempt: 1, maxAttempts: 4, want: failureActionRetry},
		{name: "network error remains retryable", err: &url.Error{Op: "Post", URL: "https://model.example/v1", Err: context.DeadlineExceeded}, currentAttempt: 1, maxAttempts: 4, want: failureActionRetry},
		{name: "401 fails immediately", err: &openai.Error{StatusCode: http.StatusUnauthorized}, currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "403 fails immediately", err: &openai.Error{StatusCode: http.StatusForbidden}, currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "invalid parameters fail immediately", err: &openai.Error{StatusCode: http.StatusBadRequest}, currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "unprocessable parameters fail immediately", err: &openai.Error{StatusCode: http.StatusUnprocessableEntity}, currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "missing model fails immediately", err: &openai.Error{StatusCode: http.StatusNotFound}, currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "transient error enters DLQ after final attempt", err: &openai.Error{StatusCode: http.StatusBadGateway}, currentAttempt: 4, maxAttempts: 4, want: failureActionFailWithDeadLetter},
		{name: "invalid video skips DLQ", err: userInputProcessError(context.Canceled), currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithoutDeadLetter},
		{name: "invalid structured output enters DLQ immediately", err: permanentProcessError(context.Canceled), currentAttempt: 1, maxAttempts: 4, want: failureActionFailWithDeadLetter},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decideFailureAction(test.err, test.currentAttempt, test.maxAttempts); got != test.want {
				t.Fatalf("decideFailureAction() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRequiredTasksIncludeDecoupledEmbeddingStage(t *testing.T) {
	required, err := requiredTaskTypesForUploadMethod(queue.UploadMethodLocalDirect)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{model.TaskASR, model.TaskVision, model.TaskRAGIndex, model.TaskEmbedding, model.TaskSummary}
	if !reflect.DeepEqual(required, want) {
		t.Fatalf("required tasks = %#v, want %#v", required, want)
	}
}

func TestNextTaskTypesFollowRAGThenParallelEmbeddingAndSummary(t *testing.T) {
	if got := nextTaskTypesForCompletion(model.TaskVision, false); got != nil {
		t.Fatalf("vision before ASR completion created tasks: %#v", got)
	}
	if got := nextTaskTypesForCompletion(model.TaskVision, true); !reflect.DeepEqual(got, []string{model.TaskRAGIndex}) {
		t.Fatalf("ASR/Vision completion tasks = %#v", got)
	}
	want := []string{model.TaskEmbedding, model.TaskSummary}
	if got := nextTaskTypesForCompletion(model.TaskRAGIndex, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("RAG completion tasks = %#v, want %#v", got, want)
	}
}

func TestDeriveVideoStatusPreservesPausedWithPendingJobs(t *testing.T) {
	jobs := []model.ProcessingJob{
		{TaskType: model.TaskASR, Status: model.JobStatusSucceeded},
		{TaskType: model.TaskVision, Status: model.JobStatusPending},
	}
	required := []string{model.TaskASR, model.TaskVision}
	status, errorMessage := deriveVideoStatus(model.VideoStatusPaused, queue.UploadMethodLocalDirect, jobs, required)
	if status != model.VideoStatusPaused || errorMessage != "" {
		t.Fatalf("deriveVideoStatus() = (%q, %q), want paused without error", status, errorMessage)
	}
}

func TestDeriveVideoStatusLetsReadyWinOverPaused(t *testing.T) {
	jobs := []model.ProcessingJob{
		{TaskType: model.TaskASR, Status: model.JobStatusSucceeded},
		{TaskType: model.TaskVision, Status: model.JobStatusSucceeded},
	}
	required := []string{model.TaskASR, model.TaskVision}
	status, _ := deriveVideoStatus(model.VideoStatusPaused, queue.UploadMethodLocalDirect, jobs, required)
	if status != model.VideoStatusReady {
		t.Fatalf("deriveVideoStatus() = %q, want ready", status)
	}
}

func TestDeriveVideoStatusLetsFailureWinOverPaused(t *testing.T) {
	jobs := []model.ProcessingJob{{TaskType: model.TaskASR, Status: model.JobStatusFailed, ErrorMessage: "ASR unavailable"}}
	status, errorMessage := deriveVideoStatus(model.VideoStatusPaused, queue.UploadMethodLocalDirect, jobs, []string{model.TaskASR})
	if status != model.VideoStatusFailed || errorMessage != "ASR unavailable" {
		t.Fatalf("deriveVideoStatus() = (%q, %q), want failed with job error", status, errorMessage)
	}
}

func TestDeriveVideoStatusKeepsURLPrepareUploading(t *testing.T) {
	jobs := []model.ProcessingJob{{TaskType: model.TaskPrepare, Status: model.JobStatusPending}}
	status, _ := deriveVideoStatus(model.VideoStatusUploading, queue.UploadMethodURL, jobs, []string{model.TaskPrepare})
	if status != model.VideoStatusUploading {
		t.Fatalf("deriveVideoStatus() = %q, want uploading", status)
	}
}

func TestBuildAudioChunksKeepsShortVideoAsTailChunk(t *testing.T) {
	chunks := buildAudioChunks(22000)
	want := []audioChunk{{StartMS: 0, EndMS: 22000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestBuildAudioChunksUsesExactSixtySecondChunk(t *testing.T) {
	chunks := buildAudioChunks(60000)
	want := []audioChunk{{StartMS: 0, EndMS: 60000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestBuildAudioChunksKeepsRemainder(t *testing.T) {
	chunks := buildAudioChunks(125000)
	want := []audioChunk{{StartMS: 0, EndMS: 60000}, {StartMS: 60000, EndMS: 120000}, {StartMS: 120000, EndMS: 125000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestASRClientSendsAudioAsChatCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		var request struct {
			Model      string `json:"model"`
			ASROptions struct {
				Language string `json:"language"`
			} `json:"asr_options"`
			Messages []struct {
				Content []struct {
					InputAudio struct {
						Data   string `json:"data"`
						Format string `json:"format"`
					} `json:"input_audio"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "mimo-v2.5-asr" || request.ASROptions.Language != "zh" {
			t.Errorf("request = %#v", request)
		}
		if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 {
			t.Fatalf("unexpected message content: %#v", request.Messages)
		}
		audio := request.Messages[0].Content[0].InputAudio
		if audio.Format != "wav" || !strings.HasPrefix(audio.Data, "data:audio/wav;base64,") {
			t.Errorf("input audio = %#v", audio)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":0,"model":"mimo-v2.5-asr","choices":[{"index":0,"message":{"role":"assistant","content":"转写文本"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	client := openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL+"/v1"))
	completion, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "mimo-v2.5-asr",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{Data: "data:audio/wav;base64,AA==", Format: "wav"}),
			}),
		},
	}, option.WithJSONSet("asr_options", map[string]string{"language": "zh"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "转写文本" {
		t.Errorf("completion = %#v", completion)
	}
}
