package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestBuildAudioChunksUsesExactThirtySecondChunk(t *testing.T) {
	chunks := buildAudioChunks(30000)
	want := []audioChunk{{StartMS: 0, EndMS: 30000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestBuildAudioChunksKeepsRemainder(t *testing.T) {
	chunks := buildAudioChunks(65000)
	want := []audioChunk{{StartMS: 0, EndMS: 30000}, {StartMS: 30000, EndMS: 60000}, {StartMS: 60000, EndMS: 65000}}
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
