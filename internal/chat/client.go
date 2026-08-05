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

func newTextClient(configName, baseURL, apiKey, modelName string, timeout time.Duration) (*textClient, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("%s_BASE_URL must be an absolute HTTP(S) URL", configName)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("%s_API_KEY is required", configName)
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, fmt.Errorf("%s_MODEL is required", configName)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("%s request timeout must be positive", configName)
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)),
		option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
		option.WithMaxRetries(2),
	)
	return &textClient{client: client, model: strings.TrimSpace(modelName)}, nil
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
