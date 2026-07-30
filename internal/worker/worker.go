package worker

import (
	"context"
	"encoding/json"
	"log"

	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/IBM/sarama"
	"gorm.io/gorm"
)

// Processor 隔离下载、FFmpeg、ASR/OCR/视觉、Embedding 与 LLM 等供应商实现。
// 实现必须幂等，因为 Kafka 可能重复投递同一个任务。
type Processor interface {
	Process(context.Context, queue.VideoJobMessage) error
}

// Consumer 将 Kafka 消息消费逻辑与数据库状态流转集中管理。
type Consumer struct {
	db        *gorm.DB
	processor Processor
}

// New 创建绑定具体视频处理器的消费者。
func New(db *gorm.DB, p Processor) *Consumer { return &Consumer{db: db, processor: p} }

// Setup 在 Consumer Group 分配分区后调用；当前无需初始化资源。
func (x *Consumer) Setup(sarama.ConsumerGroupSession) error { return nil }

// Cleanup 在分区释放前调用；当前没有消费者本地状态需要清理。
func (x *Consumer) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim 顺序处理当前分区消息，并在处理结束后提交消费位点。
func (x *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for m := range claim.Messages() {
		var job queue.VideoJobMessage
		if e := json.Unmarshal(m.Value, &job); e != nil {
			log.Printf("invalid job: %v", e)
			session.MarkMessage(m, "")
			continue
		}
		if e := x.run(session.Context(), job); e != nil {
			// 任务失败已被 run 记录到数据库；此处继续消费，防止单个任务阻塞分区。
			log.Printf("job %s failed: %v", job.JobID, e)
		}
		session.MarkMessage(m, "")
	}
	return nil
}

// run 执行一次任务并维护状态机。已成功的任务直接返回，保证重复投递安全。
func (x *Consumer) run(ctx context.Context, j queue.VideoJobMessage) error {
	var existing model.ProcessingJob
	if e := x.db.First(&existing, "id = ?", j.JobID).Error; e != nil {
		return e
	}
	if existing.Status == "succeeded" {
		return nil
	}
	if e := x.db.Model(&existing).Updates(map[string]any{"status": "running"}).Error; e != nil {
		return e
	}
	if e := x.db.Model(&model.Video{}).Where("id = ?", j.VideoID).Update("status", "downloading").Error; e != nil {
		return e
	}
	if e := x.processor.Process(ctx, j); e != nil {
		// 失败原因同时写入任务和视频，前端可从状态接口获取处理结果。
		_ = x.db.Model(&existing).Updates(map[string]any{"status": "failed", "error_message": e.Error()}).Error
		_ = x.db.Model(&model.Video{}).Where("id = ?", j.VideoID).Updates(map[string]any{"status": "failed", "error_message": e.Error()}).Error
		return e
	}
	// 成功状态需要原子更新，避免任务与视频状态出现不一致。
	return x.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Model(&existing).Update("status", "succeeded").Error; e != nil {
			return e
		}
		return tx.Model(&model.Video{}).Where("id = ?", j.VideoID).Update("status", "ready").Error
	})
}

// PlaceholderProcessor 在接入媒体和 AI 供应商前保证基础设施仍可运行。
type PlaceholderProcessor struct{}

func (PlaceholderProcessor) Process(ctx context.Context, j queue.VideoJobMessage) error {
	log.Printf("received video processing job %s for video %s", j.JobID, j.VideoID)
	return nil
}
