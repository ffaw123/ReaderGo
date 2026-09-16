package visionagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

type orchestratorClient interface {
	Next(context.Context, OrchestratorInput, []LoopMessage) (OrchestratorTurn, error)
	Model() string
}

type visionObserverClient interface {
	Observe(context.Context, ObserverInput, []Frame) (Observation, error)
	Model() string
}

type openAICompatibleClient struct {
	client openai.Client
	model  string
}

type mimoOrchestratorClient struct{ openAICompatibleClient }
type qwenObserverClient struct{ openAICompatibleClient }

var inspectVideoRangeTool = openai.ChatCompletionToolParam{
	Function: shared.FunctionDefinitionParam{
		Name:        ToolName,
		Description: openai.String("检查视频半开区间 [start_ms, end_ms) 的实际画面，并返回受验证的结构化观察；本轮尚无视觉观察时必须先调用此工具，不能直接回答"),
		Parameters: shared.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"start_ms":    map[string]any{"type": "integer", "minimum": 0},
				"end_ms":      map[string]any{"type": "integer", "minimum": 1},
				"interval_ms": map[string]any{"type": "integer", "minimum": MinIntervalMS},
			},
			"required":             []string{"start_ms", "end_ms", "interval_ms"},
			"additionalProperties": false,
		},
	},
}

func newOrchestratorClient(baseURL, apiKey, modelName string, timeout time.Duration) (*mimoOrchestratorClient, error) {
	client, err := newOpenAICompatibleClient(baseURL, apiKey, modelName, timeout, "CHAT")
	if err != nil {
		return nil, err
	}
	return &mimoOrchestratorClient{openAICompatibleClient: client}, nil
}

func newVisionObserverClient(baseURL, apiKey, modelName string, timeout time.Duration) (*qwenObserverClient, error) {
	client, err := newOpenAICompatibleClient(baseURL, apiKey, modelName, timeout, "VISION")
	if err != nil {
		return nil, err
	}
	return &qwenObserverClient{openAICompatibleClient: client}, nil
}

func newOpenAICompatibleClient(baseURL, apiKey, modelName string, timeout time.Duration, prefix string) (openAICompatibleClient, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return openAICompatibleClient{}, fmt.Errorf("%s_BASE_URL must be an absolute HTTP(S) URL", prefix)
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelName) == "" {
		return openAICompatibleClient{}, fmt.Errorf("%s_API_KEY and %s_MODEL are required", prefix, prefix)
	}
	if timeout <= 0 {
		return openAICompatibleClient{}, fmt.Errorf("%s request timeout must be positive", prefix)
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)),
		option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
		option.WithMaxRetries(2),
	)
	return openAICompatibleClient{client: client, model: strings.TrimSpace(modelName)}, nil
}

func (c *mimoOrchestratorClient) Model() string { return c.model }
func (c *qwenObserverClient) Model() string     { return c.model }

// observerVideoBlock 与 observerTextBlock 是百炼“图片列表输入视频”协议的内容块，
// 官方参考：https://help.aliyun.com/zh/model-studio/vision。
// openai-go v1.12.0 尚无 video 内容类型，最终通过 option.WithJSONSet 覆盖 messages[0].content。
type observerVideoBlock struct {
	Type  string   `json:"type"`
	Video []string `json:"video"`
	FPS   float64  `json:"fps"`
}

type observerTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// observerFPS 按抽帧间隔计算视频帧列表的 fps：fps = 1000 / interval_ms。
// 未指定间隔（如单帧）时按 1fps 排列。
func observerFPS(intervalMS int64) float64 {
	if intervalMS < MinIntervalMS {
		return 1
	}
	return float64(1000) / float64(intervalMS)
}

func (c *mimoOrchestratorClient) Next(ctx context.Context, input OrchestratorInput, history []LoopMessage) (OrchestratorTurn, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return OrchestratorTurn{}, fmt.Errorf("encode orchestrator input: %w", err)
	}
	userContent := string(payload)
	if len(history) == 0 {
		// 中文注释：MiMo 仅支持 auto，首轮通过清晰的用户层协议要求提升原生 tool_call 的优先级。
		userContent = "首轮必须调用 inspect_video_range，不要返回文字结论。上下文：\n" + userContent
	}
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(orchestratorSystemPrompt),
		openai.UserMessage(userContent),
	}
	for _, message := range history {
		switch message.Role {
		case "assistant":
			if message.Tool == nil {
				return OrchestratorTurn{}, errors.New("assistant loop message is missing tool call")
			}
			assistant := openai.ChatCompletionAssistantMessageParam{
				ToolCalls: []openai.ChatCompletionMessageToolCallParam{{
					ID: message.Tool.ID,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name: ToolName, Arguments: mustJSON(message.Tool.Arguments),
					},
				}},
			}
			if message.Content != "" {
				assistant.Content.OfString = openai.String(message.Content)
			}
			messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case "tool":
			messages = append(messages, openai.ToolMessage(message.Content, message.ToolCallID))
		default:
			return OrchestratorTurn{}, fmt.Errorf("unsupported loop role %q", message.Role)
		}
	}
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(c.model), Messages: messages, Temperature: openai.Float(0),
	}
	if !input.ForceFinal {
		params.Tools = []openai.ChatCompletionToolParam{inspectVideoRangeTool}
		params.ParallelToolCalls = openai.Bool(false)
		// MiMo 当前只支持 auto；其他 tool_choice 会被服务端移除，不能作为首轮强制机制。
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")}
	}
	started := time.Now()
	requestOptions := []option.RequestOption{
		option.WithJSONSet("thinking", map[string]string{"type": "disabled"}),
		option.WithMaxRetries(2),
	}
	if input.ForceFinal {
		// 中文注释：预算耗尽或格式修复时关闭工具并启用 JSON Mode，确保模型只生成严格 FINAL。
		requestOptions = append(requestOptions, option.WithJSONSet("response_format", map[string]string{"type": "json_object"}))
	}
	completion, err := c.client.Chat.Completions.New(ctx, params, requestOptions...)
	if err != nil {
		return OrchestratorTurn{}, fmt.Errorf("call vision orchestrator: %w", err)
	}
	if len(completion.Choices) == 0 {
		return OrchestratorTurn{}, errors.New("vision orchestrator returned no choices")
	}
	choice := completion.Choices[0]
	if choice.FinishReason == "length" {
		return OrchestratorTurn{}, ErrOutputTruncated
	}
	if len(choice.Message.ToolCalls) > 1 {
		return OrchestratorTurn{}, fmt.Errorf("orchestrator returned multiple tool calls: %w", ErrInvalidOutput)
	}
	content := strings.TrimSpace(choice.Message.Content)
	log.Printf("vision orchestrator response: model=%q duration_ms=%d finish_reason=%q tool_calls=%d", c.model, time.Since(started).Milliseconds(), choice.FinishReason, len(choice.Message.ToolCalls))
	if len(choice.Message.ToolCalls) == 1 {
		call := choice.Message.ToolCalls[0]
		if call.ID == "" || call.Function.Name != ToolName {
			return OrchestratorTurn{}, fmt.Errorf("invalid orchestrator tool call: %w", ErrInvalidOutput)
		}
		arguments, err := decodeToolArguments(call.Function.Arguments)
		if err != nil {
			return OrchestratorTurn{}, fmt.Errorf("decode orchestrator tool arguments: %w", err)
		}
		return OrchestratorTurn{Content: content, Tool: &ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments}}, nil
	}
	if content == "" {
		return OrchestratorTurn{}, errors.New("vision orchestrator returned empty content")
	}
	decision, err := decodeFinalDecision(content, nil)
	if err != nil {
		code, _ := safeOrchestratorError(err)
		log.Printf("vision orchestrator output rejected: model=%q code=%s error=%v", c.model, code, err)
		return OrchestratorTurn{}, fmt.Errorf("decode orchestrator final output: %w", err)
	}
	// observation_id 的存在性必须由持有本轮工具结果的 Agent 校验。
	return OrchestratorTurn{Content: content, Final: &decision}, nil
}

func (c *qwenObserverClient) Observe(ctx context.Context, input ObserverInput, frames []Frame) (Observation, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return Observation{}, fmt.Errorf("encode observer input: %w", err)
	}
	// Qwen3-VL Flash 在百炼兼容接口中不使用 system 角色，规则和问题必须放进同一条 user 多模态消息。
	videoURLs := make([]string, 0, len(frames))
	for _, frame := range frames {
		imageBytes, err := os.ReadFile(frame.Path)
		if err != nil {
			return Observation{}, fmt.Errorf("read vision frame: %w", err)
		}
		videoURLs = append(videoURLs, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(imageBytes))
	}
	// 中文注释：连续帧按官方“图片列表输入视频”协议放入单个 type=video 内容块；
	// fps 表达帧间隔，帮助模型理解顺序与动态变化；frames 已按采样时间升序。
	content := []any{
		observerVideoBlock{Type: "video", Video: videoURLs, FPS: observerFPS(input.IntervalMS)},
		observerTextBlock{Type: "text", Text: observerSystemPrompt + "\n\n当前请求：\n" + string(payload)},
	}
	started := time.Now()
	completion, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:       openai.ChatModel(c.model),
		Messages:    []openai.ChatCompletionMessageParamUnion{openai.UserMessage("vision observer request")},
		Temperature: openai.Float(0),
	},
		// 中文注释：占位 user message 仅用于满足 SDK 结构，序列化后用 sjson 路径整体覆盖为 video+text。
		option.WithJSONSet("messages.0.content", content),
		// 中文注释：观察任务无需思考链；直接生成纯文本可避开视觉模型不稳定的 JSON Mode。
		option.WithJSONSet("enable_thinking", false),
		option.WithMaxRetries(2),
	)
	if err != nil {
		return Observation{}, fmt.Errorf("call vision observer: %w", err)
	}
	if len(completion.Choices) == 0 {
		return Observation{}, errors.New("vision observer returned no choices")
	}
	choice := completion.Choices[0]
	if choice.FinishReason == "length" {
		return Observation{}, ErrOutputTruncated
	}
	log.Printf("vision observer response: model=%q duration_ms=%d finish_reason=%q", c.model, time.Since(started).Milliseconds(), choice.FinishReason)
	contentText := strings.TrimSpace(choice.Message.Content)
	observation, err := decodeObservation(contentText)
	if err != nil {
		code, _ := safeObserverError(err)
		// 只记录错误分类、内容长度和采样时间，不记录图片、Base64 或模型原始输出。
		log.Printf("vision observer output rejected: model=%q sampled_timestamps_ms=%v content_bytes=%d code=%s error=%v", c.model, timestampsOf(frames), len(contentText), code, err)
		return Observation{}, fmt.Errorf("validate vision observation: %w", err)
	}
	return observation, nil
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
