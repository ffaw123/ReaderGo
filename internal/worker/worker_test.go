package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"readergo/internal/media"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func TestBuildAudioChunksPrefersSilenceNearTenSeconds(t *testing.T) {
	chunks := buildAudioChunks(28000, []media.SilenceInterval{{StartMS: 9000, EndMS: 10000}, {StartMS: 19500, EndMS: 20000}})
	want := []audioChunk{{StartMS: 0, EndMS: 9000}, {StartMS: 10000, EndMS: 19500}, {StartMS: 20000, EndMS: 28000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestBuildAudioChunksSkipsLongSilenceBeforeMaximumDuration(t *testing.T) {
	chunks := buildAudioChunks(30000, []media.SilenceInterval{{StartMS: 14000, EndMS: 18000}})
	want := []audioChunk{{StartMS: 0, EndMS: 14000}, {StartMS: 18000, EndMS: 30000}}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %#v, want %#v", chunks, want)
	}
}

func TestBuildAudioChunksUsesExactRangesForHardCut(t *testing.T) {
	chunks := buildAudioChunks(22000, nil)
	want := []audioChunk{{StartMS: 0, EndMS: 10000}, {StartMS: 10000, EndMS: 22000}}
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
