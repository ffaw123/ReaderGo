package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/IBM/sarama"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Processor isolates the concrete implementation of each video-processing stage.
type Processor interface {
	Process(context.Context, queue.VideoJobMessage) error
}

// Consumer coordinates task execution while the database remains the source of truth.
type Consumer struct {
	db        *gorm.DB
	processor Processor
	jobs      *queue.Producer
}

func New(db *gorm.DB, processor Processor, jobs *queue.Producer) *Consumer {
	return &Consumer{db: db, processor: processor, jobs: jobs}
}

func (x *Consumer) Setup(sarama.ConsumerGroupSession) error { return nil }

func (x *Consumer) Cleanup(sarama.ConsumerGroupSession) error { return nil }

func (x *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for message := range claim.Messages() {
		var job queue.VideoJobMessage
		if err := json.Unmarshal(message.Value, &job); err != nil {
			log.Printf("invalid job: %v", err)
			session.MarkMessage(message, "")
			continue
		}
		if err := x.run(session.Context(), job); err != nil {
			log.Printf("job %s failed: %v", job.JobID, err)
		}
		session.MarkMessage(message, "")
	}
	return nil
}

func (x *Consumer) run(ctx context.Context, message queue.VideoJobMessage) error {
	var job model.ProcessingJob
	if err := x.db.First(&job, "id = ?", message.JobID).Error; err != nil {
		return err
	}
	if job.Status == model.JobStatusSucceeded {
		return nil
	}
	if job.TaskType != message.TaskType || job.VideoID != message.VideoID {
		return fmt.Errorf("job message does not match persisted job %s", job.ID)
	}
	if ready, err := x.prerequisitesSatisfied(&job); err != nil {
		return err
	} else if !ready {
		return fmt.Errorf("prerequisites are not complete for %s job %s", job.TaskType, job.ID)
	}
	if err := x.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusRunning, "error_message": ""}).Error; err != nil {
			return err
		}
		return tx.Model(&model.Video{}).Where("id = ?", job.VideoID).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
	}); err != nil {
		return err
	}
	if err := x.processor.Process(ctx, message); err != nil {
		_ = x.markJobFailed(job, err.Error())
		return err
	}

	var next []model.ProcessingJob
	if err := x.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusSucceeded, "error_message": ""}).Error; err != nil {
			return err
		}
		var err error
		next, err = x.createNextJobs(tx, job)
		if err != nil {
			return err
		}
		return x.reconcileVideoStatus(tx, job.VideoID)
	}); err != nil {
		return err
	}
	for _, nextJob := range next {
		if err := x.jobs.Publish(ctx, queue.VideoJobMessage{JobID: nextJob.ID, TenantID: nextJob.TenantID, VideoID: nextJob.VideoID, TaskType: nextJob.TaskType, Attempt: nextJob.Attempt}); err != nil {
			_ = x.markJobFailed(nextJob, err.Error())
			return err
		}
	}
	return nil
}

func (x *Consumer) prerequisitesSatisfied(job *model.ProcessingJob) (bool, error) {
	var prerequisiteTypes []string
	switch job.TaskType {
	case model.TaskPrepare:
		return true, nil
	case model.TaskASR, model.TaskVision:
		prerequisiteTypes = []string{model.TaskPrepare}
	case model.TaskRAGIndex, model.TaskSummary:
		prerequisiteTypes = []string{model.TaskASR, model.TaskVision}
	default:
		return false, fmt.Errorf("unknown task type %q", job.TaskType)
	}
	var count int64
	err := x.db.Model(&model.ProcessingJob{}).
		Where("video_id = ? AND task_type IN ? AND status = ?", job.VideoID, prerequisiteTypes, model.JobStatusSucceeded).
		Count(&count).Error
	return count == int64(len(prerequisiteTypes)), err
}

func (x *Consumer) createNextJobs(tx *gorm.DB, completed model.ProcessingJob) ([]model.ProcessingJob, error) {
	var taskTypes []string
	switch completed.TaskType {
	case model.TaskPrepare:
		taskTypes = []string{model.TaskASR, model.TaskVision}
	case model.TaskASR, model.TaskVision:
		var completeCount int64
		if err := tx.Model(&model.ProcessingJob{}).
			Where("video_id = ? AND task_type IN ? AND status = ?", completed.VideoID, []string{model.TaskASR, model.TaskVision}, model.JobStatusSucceeded).
			Count(&completeCount).Error; err != nil {
			return nil, err
		}
		if completeCount == 2 {
			taskTypes = []string{model.TaskRAGIndex, model.TaskSummary}
		}
	}

	next := make([]model.ProcessingJob, 0, len(taskTypes))
	for _, taskType := range taskTypes {
		candidate := model.ProcessingJob{TenantID: completed.TenantID, VideoID: completed.VideoID, TaskType: taskType, Attempt: 1, Status: model.JobStatusPending}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "video_id"}, {Name: "task_type"}},
			DoNothing: true,
		}).Create(&candidate)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			next = append(next, candidate)
		}
	}
	return next, nil
}

// reconcileVideoStatus is the only coordinator that derives videos.status from task states.
func (x *Consumer) reconcileVideoStatus(tx *gorm.DB, videoID string) error {
	var jobs []model.ProcessingJob
	if err := tx.Where("video_id = ?", videoID).Find(&jobs).Error; err != nil {
		return err
	}
	statuses := make(map[string]string, len(jobs))
	for _, job := range jobs {
		if job.Status == model.JobStatusFailed {
			return tx.Model(&model.Video{}).Where("id = ?", videoID).Updates(map[string]any{"status": model.VideoStatusFailed, "error_message": job.ErrorMessage}).Error
		}
		statuses[job.TaskType] = job.Status
	}
	for _, taskType := range model.RequiredTaskTypes {
		if statuses[taskType] != model.JobStatusSucceeded {
			return tx.Model(&model.Video{}).Where("id = ?", videoID).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
		}
	}
	return tx.Model(&model.Video{}).Where("id = ?", videoID).Updates(map[string]any{"status": model.VideoStatusReady, "error_message": ""}).Error
}

func (x *Consumer) markJobFailed(job model.ProcessingJob, message string) error {
	return x.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusFailed, "error_message": message}).Error; err != nil {
			return err
		}
		return x.reconcileVideoStatus(tx, job.VideoID)
	})
}

// PlaceholderProcessor keeps the pipeline runnable until concrete media and AI processors are wired in.
type PlaceholderProcessor struct{}

func (PlaceholderProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	log.Printf("received %s job %s for video %s", job.TaskType, job.JobID, job.VideoID)
	return nil
}
