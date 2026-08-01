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
)

var VideoJobsTopics = []string{PrepareJobsTopic, ASRJobsTopic, VisionJobsTopic, RAGIndexJobsTopic, SummaryJobsTopic}

// VideoJobMessage 包含定位并处理一个视频任务所需的最小信息。
type VideoJobMessage struct {
	JobID    string `json:"job_id"`
	TenantID string `json:"tenant_id"`
	VideoID  string `json:"video_id"`
	TaskType string `json:"task_type"`
	Attempt  int    `json:"attempt"`
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

// Publish 序列化任务，并以视频 ID 作为 Key 以尽量维持同视频任务的顺序。
func (p *Producer) Publish(ctx context.Context, m VideoJobMessage) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	topic, e := TopicForTask(m.TaskType)
	if e != nil {
		return e
	}
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
