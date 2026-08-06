package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type textClient struct {
	client openai.Client
	model  string
}

// streamingTextClient 专用于面向用户的回答生成；意图识别继续使用 textClient。
type streamingTextClient struct {
	client openai.Client
	model  string
}

func newTextClient(configName, baseURL, apiKey, modelName string, timeout time.Duration) (*textClient, error) {
	client, model, err := newOpenAIClient(configName, baseURL, apiKey, modelName, timeout)
	if err != nil {
		return nil, err
	}
	return &textClient{client: client, model: model}, nil
}

func newStreamingTextClient(configName, baseURL, apiKey, modelName string, timeout time.Duration) (*streamingTextClient, error) {
	client, model, err := newOpenAIClient(configName, baseURL, apiKey, modelName, timeout)
	if err != nil {
		return nil, err
	}
	return &streamingTextClient{client: client, model: model}, nil
}

func newOpenAIClient(configName, baseURL, apiKey, modelName string, timeout time.Duration) (openai.Client, string, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return openai.Client{}, "", fmt.Errorf("%s_BASE_URL must be an absolute HTTP(S) URL", configName)
	}
	if strings.TrimSpace(apiKey) == "" {
		return openai.Client{}, "", fmt.Errorf("%s_API_KEY is required", configName)
	}
	if strings.TrimSpace(modelName) == "" {
		return openai.Client{}, "", fmt.Errorf("%s_MODEL is required", configName)
	}
	if timeout <= 0 {
		return openai.Client{}, "", fmt.Errorf("%s request timeout must be positive", configName)
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)),
		option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
		option.WithMaxRetries(2),
	)
	return client, strings.TrimSpace(modelName), nil
}

func (c *textClient) complete(ctx context.Context, systemPrompt, userPayload string, jsonMode bool) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPayload),
		},
	}
	options := []option.RequestOption{option.WithMaxRetries(2)}
	if jsonMode {
		options = append(options, option.WithJSONSet("response_format", map[string]string{"type": "json_object"}))
	}
	completion, err := c.client.Chat.Completions.New(ctx, params, options...)
	if err != nil {
		return "", err
	}
	if len(completion.Choices) == 0 {
		return "", errors.New("chat model returned no choices")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("chat model returned empty content")
	}
	return content, nil
}

func (c *streamingTextClient) complete(ctx context.Context, systemPrompt, userPayload string, jsonMode bool, onDelta func(string) error) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(c.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPayload),
		},
	}
	options := []option.RequestOption{option.WithMaxRetries(2)}
	if jsonMode {
		options = append(options, option.WithJSONSet("response_format", map[string]string{"type": "json_object"}))
	}

	stream := c.client.Chat.Completions.NewStreaming(ctx, params, options...)
	defer stream.Close()
	var content strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			delta := choice.Delta.Content
			if delta == "" {
				continue
			}
			content.WriteString(delta)
			// 回调失败通常表示浏览器已经断开，此时立即停止上游模型请求。
			if onDelta != nil {
				if err := onDelta(delta); err != nil {
					return "", err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", errors.New("chat model returned empty content")
	}
	return result, nil
}
