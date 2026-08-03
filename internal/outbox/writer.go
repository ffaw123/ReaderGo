package outbox

import (
	"encoding/json"
	"fmt"
	"time"

	"readergo/internal/model"
	"readergo/internal/queue"

	"gorm.io/gorm"
)

// Writer 只负责在调用方的数据库事务内追加 Outbox，不直接进行任何网络发布。
type Writer struct {
	jobsExchange string
	dlxExchange  string
}

func NewWriter(jobsExchange, dlxExchange string) *Writer {
	return &Writer{jobsExchange: jobsExchange, dlxExchange: dlxExchange}
}

func (w *Writer) EnqueueJob(tx *gorm.DB, job model.ProcessingJob, uploadMethod string, availableAt time.Time) error {
	return w.enqueue(tx, job, uploadMethod, w.jobsExchange, availableAt)
}

func (w *Writer) EnqueueDeadLetter(tx *gorm.DB, job model.ProcessingJob, uploadMethod string) error {
	return w.enqueue(tx, job, uploadMethod, w.dlxExchange, time.Now().UTC())
}

func (w *Writer) enqueue(tx *gorm.DB, job model.ProcessingJob, uploadMethod, exchange string, availableAt time.Time) error {
	routingKey, err := queue.RoutingKeyForTask(job.TaskType)
	if err != nil {
		return err
	}
	message := queue.VideoJobMessage{
		JobID: job.ID, TenantID: job.TenantID, VideoID: job.VideoID,
		TaskType: job.TaskType, Attempt: job.Attempt, UploadMethod: uploadMethod,
	}
	if err := queue.ValidateMessage(message); err != nil {
		return err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	record := model.OutboxMessage{
		TenantID: job.TenantID, VideoID: job.VideoID, JobID: job.ID, TaskType: job.TaskType,
		Attempt: job.Attempt, ExchangeName: exchange, RoutingKey: routingKey, Payload: string(payload),
		Status: model.OutboxStatusPending, AvailableAt: availableAt.UTC(), LastError: "",
	}
	return tx.Create(&record).Error
}
