package visionagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type modelClient interface {
	Decide(context.Context, decisionInput, []Frame, map[int64]Frame) (Decision, error)
	Answer(context.Context, answerInput, map[int64]Frame) (FinalAnswer, error)
	Model() string
}

type vllmClient struct {
	client openai.Client
	model  string
}

func newVLLMClient(baseURL, apiKey, modelName string, timeout time.Duration) (*vllmClient, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("VISION_BASE_URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelName) == "" {
		return nil, errors.New("VISION_API_KEY and VISION_MODEL are required")
	}
	if timeout <= 0 {
		return nil, errors.New("VISION request timeout must be positive")
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)), option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}), option.WithMaxRetries(2),
	)
	return &vllmClient{client: client, model: strings.TrimSpace(modelName)}, nil
}

func (c *vllmClient) Model() string { return c.model }

func (c *vllmClient) Decide(ctx context.Context, input decisionInput, frames []Frame, sampled map[int64]Frame) (Decision, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return Decision{}, fmt.Errorf("encode vision decision input: %w", err)
	}
	parts := []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(string(payload))}
	for _, frame := range frames {
		imageBytes, err := os.ReadFile(frame.Path)
		if err != nil {
			return Decision{}, fmt.Errorf("read vision frame: %w", err)
		}
		// 时间标签必须紧邻对应图片，模型不得依赖文件名推断时间。
		parts = append(parts,
			openai.TextContentPart(fmt.Sprintf("FRAME timestamp_ms=%d display_time=%s", frame.TimestampMS, frame.DisplayTime)),
			openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageBytes),
			}),
		)
	}
	content, err := c.complete(ctx, agentSystemPrompt, openai.UserMessage(parts))
	if err != nil {
		return Decision{}, err
	}
	decision, err := decodeDecision(content, sampled)
	if err != nil {
		return Decision{}, fmt.Errorf("decode vision decision: %w", err)
	}
	return decision, nil
}

func (c *vllmClient) Answer(ctx context.Context, input answerInput, sampled map[int64]Frame) (FinalAnswer, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return FinalAnswer{}, fmt.Errorf("encode vision answer input: %w", err)
	}
	content, err := c.complete(ctx, answerSystemPrompt, openai.UserMessage(string(payload)))
	if err != nil {
		return FinalAnswer{}, err
	}
	answer, err := decodeFinalAnswer(content, sampled)
	if err != nil {
		return FinalAnswer{}, fmt.Errorf("decode vision answer: %w", err)
	}
	return answer, nil
}

func (c *vllmClient) complete(ctx context.Context, systemPrompt string, user openai.ChatCompletionMessageParamUnion) (string, error) {
	completion, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt), user},
	}, option.WithJSONSet("response_format", map[string]string{"type": "json_object"}), option.WithMaxRetries(2))
	if err != nil {
		return "", fmt.Errorf("call vision model: %w", err)
	}
	if len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return "", errors.New("vision model returned empty content")
	}
	return strings.TrimSpace(completion.Choices[0].Message.Content), nil
}
