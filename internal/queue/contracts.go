package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	RoutingKeyPrepare   = "video.prepare"
	RoutingKeyASR       = "video.asr"
	RoutingKeyVision    = "video.vision"
	RoutingKeyRAGIndex  = "video.rag_index"
	RoutingKeyEmbedding = "video.embedding"
	RoutingKeySummary   = "video.summary"

	UploadMethodLocalDirect = "local_direct"
	UploadMethodURL         = "url"
)

// VideoJobMessage 只携带任务定位信息，媒体和处理结果始终保存在对象存储或 PostgreSQL。
type VideoJobMessage struct {
	JobID        string `json:"job_id"`
	TenantID     string `json:"tenant_id"`
	VideoID      string `json:"video_id"`
	TaskType     string `json:"task_type"`
	Attempt      int    `json:"attempt"`
	UploadMethod string `json:"upload_method"`
}

// PublishMessage 是 Dispatcher 交给 Confirm Publisher 的完整 AMQP 信封。
type PublishMessage struct {
	MessageID  string
	Exchange   string
	RoutingKey string
	Body       []byte
}

type Publisher interface {
	Publish(ctx context.Context, message PublishMessage) error
	Close() error
}

type Handler interface {
	Handle(ctx context.Context, message VideoJobMessage) error
}

// PermanentError 表示毒消息；Consumer 会拒绝且不重入主队列，由 DLX 接管。
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

func IsPermanent(err error) bool {
	var target *PermanentError
	return errors.As(err, &target)
}

func IsUploadMethod(method string) bool {
	return method == UploadMethodLocalDirect || method == UploadMethodURL
}

func RoutingKeyForTask(taskType string) (string, error) {
	switch taskType {
	case "prepare":
		return RoutingKeyPrepare, nil
	case "asr":
		return RoutingKeyASR, nil
	case "vision":
		return RoutingKeyVision, nil
	case "rag_index":
		return RoutingKeyRAGIndex, nil
	case "embedding":
		return RoutingKeyEmbedding, nil
	case "summary":
		return RoutingKeySummary, nil
	default:
		return "", fmt.Errorf("unknown task type %q", taskType)
	}
}

func ValidateMessage(message VideoJobMessage) error {
	if message.JobID == "" || message.TenantID == "" || message.VideoID == "" || message.Attempt < 1 {
		return fmt.Errorf("job message is missing required locator fields")
	}
	if !IsUploadMethod(message.UploadMethod) {
		return fmt.Errorf("unknown upload method %q", message.UploadMethod)
	}
	_, err := RoutingKeyForTask(message.TaskType)
	return err
}

func DecodeMessage(body []byte) (VideoJobMessage, error) {
	var message VideoJobMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return message, fmt.Errorf("decode job message: %w", err)
	}
	if err := ValidateMessage(message); err != nil {
		return message, err
	}
	return message, nil
}
