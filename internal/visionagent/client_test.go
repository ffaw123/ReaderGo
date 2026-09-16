package visionagent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchestratorUsesNativeToolCallAndDisablesThinking(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"mimo-v2.5-pro","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"需要细看","tool_calls":[{"id":"call-1","type":"function","function":{"name":"inspect_video_range","arguments":"{\"start_ms\":1000,\"end_ms\":2000,\"interval_ms\":500}"}}]}}]}`))
	}))
	defer server.Close()

	client, err := newOrchestratorClient(server.URL+"/v1", "test-key", "mimo-v2.5-pro", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := client.Next(context.Background(), OrchestratorInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Tool == nil || turn.Tool.ID != "call-1" || turn.Tool.Arguments.IntervalMS != 500 {
		t.Fatalf("unexpected turn: %#v", turn)
	}
	thinking, ok := request["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v", request["thinking"])
	}
	if parallel, ok := request["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("parallel_tool_calls = %#v", request["parallel_tool_calls"])
	}
	if request["tool_choice"] != "auto" {
		t.Fatalf("first tool_choice = %#v, want auto", request["tool_choice"])
	}
	if _, exists := request["response_format"]; exists {
		t.Fatalf("orchestrator request must not combine JSON mode with native tool calls: %#v", request["response_format"])
	}
}

func TestOrchestratorForceFinalUsesJSONModeWithoutTools(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"mimo-v2.5-pro","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"action\":\"FINAL\",\"evidence_status\":\"NO_EVIDENCE\",\"answer\":\"没有足够证据。\",\"used_observation_ids\":[]}"}}]}`))
	}))
	defer server.Close()

	client, err := newOrchestratorClient(server.URL+"/v1", "test-key", "mimo-v2.5-pro", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := client.Next(context.Background(), OrchestratorInput{ForceFinal: true}, []LoopMessage{
		{Role: "assistant", Tool: &ToolCall{ID: "call-1", Name: ToolName, Arguments: InspectVideoRangeInput{StartMS: 0, EndMS: 1_000, IntervalMS: 500}}},
		{Role: "tool", ToolCallID: "call-1", Content: `{"ok":true,"observation_id":"obs-1","evidence_status":"NO_EVIDENCE"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Final == nil || turn.Final.EvidenceStatus != "NO_EVIDENCE" {
		t.Fatalf("unexpected final turn: %#v", turn)
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("forced final request unexpectedly contains tools: %#v", request["tools"])
	}
	responseFormat, ok := request["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_object" {
		t.Fatalf("response_format = %#v, want json_object", request["response_format"])
	}
}

func observerFrameFiles(t *testing.T, contents ...string) []Frame {
	t.Helper()
	dir := t.TempDir()
	frames := make([]Frame, 0, len(contents))
	for index, content := range contents {
		path := filepath.Join(dir, fmt.Sprintf("frame-%d.jpg", index))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, Frame{
			TimestampMS: int64(1000 * (index + 1)), DisplayTime: fmt.Sprintf("00:00:%02d.000", index+1), Path: path,
		})
	}
	return frames
}

func decodeObserverFrameDataURI(t *testing.T, value any) []byte {
	t.Helper()
	uri, ok := value.(string)
	if !ok {
		t.Fatalf("frame entry = %#v, want data URI string", value)
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("frame entry = %q, want %q prefix", uri, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
	if err != nil {
		t.Fatalf("decode frame data URI: %v", err)
	}
	return decoded
}

func TestObserverSendsVideoFrameListWithPromptAndQuestion(t *testing.T) {
	var request map[string]any
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		var err error
		if rawBody, err = io.ReadAll(incoming.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(rawBody, &request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"qwen3-vl-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"拍摄者打开车门并进入车内。"}}]}`))
	}))
	defer server.Close()

	client, err := newVisionObserverClient(server.URL+"/v1", "test-key", "qwen3-vl-flash", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	frames := observerFrameFiles(t, "jpeg-frame-a", "jpeg-frame-b", "jpeg-frame-c")
	observation, err := client.Observe(context.Background(), ObserverInput{Question: "画面里发生了什么？", IntervalMS: 1_000}, frames)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Observation != "拍摄者打开车门并进入车内。" {
		t.Fatalf("observation = %#v", observation)
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("observer must not receive tools: %#v", request["tools"])
	}
	if thinking, ok := request["enable_thinking"].(bool); !ok || thinking {
		t.Fatalf("observer enable_thinking = %#v, want false", request["enable_thinking"])
	}
	if _, exists := request["response_format"]; exists {
		t.Fatalf("plain-text observer request unexpectedly contains response_format: %#v", request["response_format"])
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("observer messages = %#v, want one user message", request["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" {
		t.Fatalf("observer must not use system role: %#v", messages[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("observer content = %#v, want a video block and a text block", message["content"])
	}
	videoBlock, ok := content[0].(map[string]any)
	if !ok || videoBlock["type"] != "video" {
		t.Fatalf("content[0] = %#v, want type=video", content[0])
	}
	videoURLs, ok := videoBlock["video"].([]any)
	if !ok || len(videoURLs) != len(frames) {
		t.Fatalf("video block frames = %#v, want %d entries", videoBlock["video"], len(frames))
	}
	for index, frame := range frames {
		expected, err := os.ReadFile(frame.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeObserverFrameDataURI(t, videoURLs[index]); !bytes.Equal(got, expected) {
			t.Fatalf("video block entry %d does not match sampled frame order: got %q, want %q", index, got, expected)
		}
	}
	if fps, ok := videoBlock["fps"].(float64); !ok || fps != 1 {
		t.Fatalf("video block fps = %#v, want 1 for 1000ms interval", videoBlock["fps"])
	}
	textBlock, ok := content[1].(map[string]any)
	if !ok || textBlock["type"] != "text" {
		t.Fatalf("content[1] = %#v, want type=text", content[1])
	}
	text, ok := textBlock["text"].(string)
	if !ok || !strings.Contains(text, "画面里发生了什么？") || !strings.Contains(text, "可信观察任务") {
		t.Fatalf("text block = %#v, want prompt and question", textBlock["text"])
	}
	if strings.Contains(string(rawBody), "image_url") {
		t.Fatalf("observer request must not contain standalone image_url parts: %s", rawBody)
	}
}

func TestObserverVideoBlockFPSMatchesSamplingInterval(t *testing.T) {
	cases := []struct {
		name       string
		intervalMS int64
		wantFPS    float64
	}{
		{name: "1000ms", intervalMS: 1_000, wantFPS: 1},
		{name: "500ms", intervalMS: 500, wantFPS: 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
				if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"qwen3-vl-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"人物动作明显。"}}]}`))
			}))
			defer server.Close()

			client, err := newVisionObserverClient(server.URL+"/v1", "test-key", "qwen3-vl-flash", 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			frames := observerFrameFiles(t, "jpeg-frame-1", "jpeg-frame-2")
			if _, err := client.Observe(context.Background(), ObserverInput{Question: "人物在做什么？", IntervalMS: testCase.intervalMS}, frames); err != nil {
				t.Fatal(err)
			}
			content := request["messages"].([]any)[0].(map[string]any)["content"].([]any)
			videoBlock, ok := content[0].(map[string]any)
			if !ok || videoBlock["type"] != "video" {
				t.Fatalf("content[0] = %#v, want type=video", content[0])
			}
			fps, ok := videoBlock["fps"].(float64)
			if !ok || fps != testCase.wantFPS {
				t.Fatalf("fps = %#v, want %v for %dms interval", videoBlock["fps"], testCase.wantFPS, testCase.intervalMS)
			}
		})
	}
}
