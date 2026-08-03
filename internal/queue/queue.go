package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/IBM/sarama"
)

// VideoJobsTopic 是 API 请求处理与视频 Worker 之间的可靠交接主题。
const (
	PrepareJobsTopic  = "video.prepare.jobs"
	ASRJobsTopic      = "video.asr.jobs"
	VisionJobsTopic   = "video.vision.jobs"
	RAGIndexJobsTopic = "video.rag-index.jobs"
	SummaryJobsTopic  = "video.summary.jobs"

	// UploadMethodLocalDirect 表示源文件已经由客户端直传至 MinIO，Worker 可直接开始媒体处理。
	UploadMethodLocalDirect = "local_direct"
	// UploadMethodURL 表示源文件仍需由 prepare 阶段下载、校验并写入对象存储。
	UploadMethodURL = "url"
)

var VideoJobsTopics = []string{PrepareJobsTopic, ASRJobsTopic, VisionJobsTopic, RAGIndexJobsTopic, SummaryJobsTopic}

// VideoJobMessage 包含定位并处理一个视频任务所需的最小信息。
type VideoJobMessage struct {
	JobID    string `json:"job_id"`
	TenantID string `json:"tenant_id"`
	VideoID  string `json:"video_id"`
	TaskType string `json:"task_type"`
	Attempt  int    `json:"attempt"`
	// UploadMethod 仅描述源文件的进入方式，用于确定 prepare 是否为前置阶段。
	UploadMethod string `json:"upload_method"`
}

// Publisher 抽象任务投递能力，便于 API 与 Worker 在测试中替换 Kafka 实现。
type Publisher interface {
	Publish(context.Context, VideoJobMessage) error
}

// IsUploadMethod 用于在消费前拒绝缺失或未知的上传方式，避免错误地跳过前置任务。
func IsUploadMethod(method string) bool {
	return method == UploadMethodLocalDirect || method == UploadMethodURL
}

// Producer 是供 HTTP 处理器使用的轻量 Kafka 封装。
type Producer struct{ client sarama.SyncProducer }

// NewProducer 创建同步生产者，使入队失败能立即反馈给 API。
func NewProducer(brokers []string) (*Producer, error) {
	p, e := sarama.NewSyncProducer(brokers, nil)
	return &Producer{p}, e
}

// Close 在优雅关闭时刷新并释放 Kafka 生产者。
func (p *Producer) Close() error { return p.client.Close() }

// Publish 投递任务。
func (p *Producer) Publish(ctx context.Context, m VideoJobMessage) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	topic, e := TopicForTask(m.TaskType)
	if e != nil {
		return e
	}
	// 使用video_id作为key确保同一个视频能进入同一个分区，保证任务的处理顺序
	_, _, e = p.client.SendMessage(&sarama.ProducerMessage{Topic: topic, Key: sarama.StringEncoder(m.VideoID), Value: sarama.ByteEncoder(b)})
	return e
}

func TopicForTask(taskType string) (string, error) {
	switch taskType {
	case "prepare":
		return PrepareJobsTopic, nil
	case "asr":
		return ASRJobsTopic, nil
	case "vision":
		return VisionJobsTopic, nil
	case "rag_index":
		return RAGIndexJobsTopic, nil
	case "summary":
		return SummaryJobsTopic, nil
	default:
		return "", fmt.Errorf("unknown task type %q", taskType)
	}
}
