package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/IBM/sarama"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const pauseStatePollInterval = 750 * time.Millisecond

// Processor 隔离每个视频处理阶段的具体实现。
// 不同阶段（prepare / asr / vision / rag_index / summary）可分别实现此接口，
// 使消费者循环与具体处理逻辑解耦，便于独立测试和替换。
type Processor interface {
	Process(context.Context, queue.VideoJobMessage) error
}

// Consumer 协调任务执行，数据库是唯一的状态来源（source of truth）。
// 持有 Processor 完成具体处理，持有 Publisher 将下一阶段任务写回 Kafka。
type Consumer struct {
	db        *gorm.DB
	processor Processor
	jobs      queue.Publisher
}

// New 创建消费者实例，绑定数据库、处理逻辑和 Kafka 投递能力。
func New(db *gorm.DB, processor Processor, jobs queue.Publisher) *Consumer {
	return &Consumer{db: db, processor: processor, jobs: jobs}
}

func (x *Consumer) Setup(sarama.ConsumerGroupSession) error   { return nil }
func (x *Consumer) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim 每条消息都以数据库任务为准；暂停时消息可以提交，恢复接口会重新投递 pending 任务。
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
		// Kafka 只负责通知，任务是否需要恢复由 PostgreSQL 中的 pending 状态决定。
		session.MarkMessage(message, "")
	}
	return nil
}

func (x *Consumer) run(ctx context.Context, message queue.VideoJobMessage) error {
	job, claimed, err := x.claimJob(ctx, message)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	// API 与 Worker 是不同进程，通过轮询数据库把 paused 转换为 Processor 可感知的 context 取消。
	processorCtx, stopPauseMonitor, pauseRequested := x.withPauseCancellation(ctx, job.VideoID)
	processErr := x.processor.Process(processorCtx, message)
	stopPauseMonitor()
	if processErr != nil {
		if pauseRequested.Load() {
			if err := x.returnJobToPendingAfterPause(job); err != nil {
				return fmt.Errorf("return paused job %s to pending: %w", job.ID, err)
			}
			return nil
		}
		_ = x.markJobFailed(job, processErr.Error(), message.UploadMethod)
		return processErr
	}

	var next []model.ProcessingJob
	publishNext := false
	if err := x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与 pause/resume 接口统一先锁视频行，避免成功提交越过暂停边界。
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ?", job.ID, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSucceeded, "error_message": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("job %s is no longer running", job.ID)
		}
		var err error
		next, err = x.createNextJobs(tx, job)
		if err != nil {
			return err
		}
		finalStatus, err := x.reconcileVideoStatusLocked(tx, &video, message.UploadMethod)
		if err != nil {
			return err
		}
		// 暂停期间可以确认当前阶段成功并创建下游 pending 任务，但不能继续向 Kafka 推进。
		publishNext = finalStatus != model.VideoStatusPaused
		return nil
	}); err != nil {
		return err
	}
	if !publishNext {
		return nil
	}
	for _, nextJob := range next {
		message := queue.VideoJobMessage{
			JobID: nextJob.ID, TenantID: nextJob.TenantID, VideoID: nextJob.VideoID,
			TaskType: nextJob.TaskType, Attempt: nextJob.Attempt, UploadMethod: message.UploadMethod,
		}
		if err := x.jobs.Publish(ctx, message); err != nil {
			_ = x.markJobFailed(nextJob, err.Error(), message.UploadMethod)
			return err
		}
	}
	return nil
}

// claimJob 使用视频行锁和条件更新完成原子抢占，重复 Kafka 消息只能有一个进入 Processor。
func (x *Consumer) claimJob(ctx context.Context, message queue.VideoJobMessage) (model.ProcessingJob, bool, error) {
	var job model.ProcessingJob
	claimed := false
	if !queue.IsUploadMethod(message.UploadMethod) {
		return job, false, fmt.Errorf("job message has unknown upload method %q", message.UploadMethod)
	}
	err := x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", message.VideoID, message.TenantID).
			First(&video).Error; err != nil {
			return err
		}
		if video.UploadMethod != message.UploadMethod {
			return fmt.Errorf("job message upload method %q does not match video %q", message.UploadMethod, video.UploadMethod)
		}
		if err := tx.First(&job, "id = ?", message.JobID).Error; err != nil {
			return err
		}
		if job.TaskType != message.TaskType || job.VideoID != message.VideoID || job.TenantID != message.TenantID || job.Attempt != message.Attempt {
			return fmt.Errorf("job message does not match persisted job %s", job.ID)
		}
		if video.Status == model.VideoStatusPaused || video.Status == model.VideoStatusReady || video.Status == model.VideoStatusFailed {
			return nil
		}
		if job.Status != model.JobStatusPending {
			return nil
		}
		ready, err := x.prerequisitesSatisfied(tx, &job, message.UploadMethod)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("prerequisites are not complete for %s job %s", job.TaskType, job.ID)
		}
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ?", job.ID, model.JobStatusPending).
			Updates(map[string]any{"status": model.JobStatusRunning, "error_message": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		job.Status = model.JobStatusRunning
		claimed = true
		if job.TaskType == model.TaskPrepare {
			return nil
		}
		return tx.Model(&video).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
	})
	return job, claimed, err
}

// withPauseCancellation 监听持久化暂停状态；所有媒体命令和外部 HTTP 请求都会继承返回的 context。
func (x *Consumer) withPauseCancellation(parent context.Context, videoID string) (context.Context, func(), *atomic.Bool) {
	processorCtx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	pauseRequested := &atomic.Bool{}
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(pauseStatePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-parent.Done():
				return
			case <-processorCtx.Done():
				return
			case <-ticker.C:
				var video model.Video
				if err := x.db.WithContext(processorCtx).Select("status").First(&video, "id = ?", videoID).Error; err != nil {
					if processorCtx.Err() != nil {
						return
					}
					log.Printf("check pause state for video %s: %v", videoID, err)
					continue
				}
				if video.Status == model.VideoStatusPaused {
					pauseRequested.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	stopMonitor := func() {
		close(stop)
		cancel()
		<-stopped
	}
	return processorCtx, stopMonitor, pauseRequested
}

func (x *Consumer) returnJobToPendingAfterPause(job model.ProcessingJob) error {
	return x.db.Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		// 兄弟任务可能在暂停期间先进入 failed；本任务仍必须退出 running，避免留下僵尸任务。
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ?", job.ID, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusPending, "error_message": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("paused job %s is no longer running", job.ID)
		}
		return nil
	})
}

func (x *Consumer) prerequisitesSatisfied(db *gorm.DB, job *model.ProcessingJob, uploadMethod string) (bool, error) {
	var prerequisiteTypes []string
	switch job.TaskType {
	case model.TaskPrepare:
		return true, nil
	case model.TaskASR, model.TaskVision:
		if uploadMethod == queue.UploadMethodURL {
			prerequisiteTypes = []string{model.TaskPrepare}
		}
	case model.TaskRAGIndex, model.TaskSummary:
		prerequisiteTypes = []string{model.TaskASR, model.TaskVision}
	default:
		return false, fmt.Errorf("unknown task type %q", job.TaskType)
	}
	if len(prerequisiteTypes) == 0 {
		return true, nil
	}
	var count int64
	err := db.Model(&model.ProcessingJob{}).
		Where("video_id = ? AND task_type IN ? AND status = ?", job.VideoID, prerequisiteTypes, model.JobStatusSucceeded).
		Count(&count).Error
	return count == int64(len(prerequisiteTypes)), err
}

// createNextJobs 只创建不存在的下一阶段任务；暂停时这些任务留在 pending，等待恢复接口重新投递。
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

// reconcileVideoStatus 是唯一负责从任务状态推导视频总体状态的地方。
func (x *Consumer) reconcileVideoStatus(tx *gorm.DB, videoID, uploadMethod string) (string, error) {
	var video model.Video
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", videoID).Error; err != nil {
		return "", err
	}
	return x.reconcileVideoStatusLocked(tx, &video, uploadMethod)
}

func (x *Consumer) reconcileVideoStatusLocked(tx *gorm.DB, video *model.Video, uploadMethod string) (string, error) {
	requiredTaskTypes, err := requiredTaskTypesForUploadMethod(uploadMethod)
	if err != nil {
		return "", err
	}
	var jobs []model.ProcessingJob
	if err := tx.Where("video_id = ?", video.ID).Find(&jobs).Error; err != nil {
		return "", err
	}
	status, errorMessage := deriveVideoStatus(video.Status, uploadMethod, jobs, requiredTaskTypes)
	updates := map[string]any{"status": status, "error_message": errorMessage}
	if status == model.VideoStatusReady || status == model.VideoStatusFailed {
		updates["paused_from_status"] = nil
	}
	if err := tx.Model(video).Updates(updates).Error; err != nil {
		return "", err
	}
	video.Status = status
	video.ErrorMessage = errorMessage
	return status, nil
}

// deriveVideoStatus 让失败和完成终态优先于 paused，其余未完成任务才保留暂停状态。
func deriveVideoStatus(currentStatus, uploadMethod string, jobs []model.ProcessingJob, requiredTaskTypes []string) (string, string) {
	statuses := make(map[string]string, len(jobs))
	for _, job := range jobs {
		if job.Status == model.JobStatusFailed {
			return model.VideoStatusFailed, job.ErrorMessage
		}
		statuses[job.TaskType] = job.Status
	}
	allSucceeded := true
	for _, taskType := range requiredTaskTypes {
		if statuses[taskType] != model.JobStatusSucceeded {
			allSucceeded = false
			break
		}
	}
	if allSucceeded {
		return model.VideoStatusReady, ""
	}
	if currentStatus == model.VideoStatusPaused {
		return model.VideoStatusPaused, ""
	}
	if uploadMethod == queue.UploadMethodURL && statuses[model.TaskPrepare] != model.JobStatusSucceeded {
		return model.VideoStatusUploading, ""
	}
	return model.VideoStatusProcessing, ""
}

func (x *Consumer) markJobFailed(job model.ProcessingJob, errorMessage, uploadMethod string) error {
	return x.db.Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		// 下游消息发布与暂停并发时保留 pending，恢复接口会负责重新投递。
		if video.Status == model.VideoStatusPaused && job.Status == model.JobStatusPending {
			return nil
		}
		if err := tx.Model(&model.ProcessingJob{}).Where("id = ?", job.ID).
			Updates(map[string]any{"status": model.JobStatusFailed, "error_message": errorMessage}).Error; err != nil {
			return err
		}
		_, err := x.reconcileVideoStatusLocked(tx, &video, uploadMethod)
		return err
	})
}

func requiredTaskTypesForUploadMethod(uploadMethod string) ([]string, error) {
	switch uploadMethod {
	case queue.UploadMethodLocalDirect:
		return []string{model.TaskASR, model.TaskVision, model.TaskRAGIndex, model.TaskSummary}, nil
	case queue.UploadMethodURL:
		return []string{model.TaskPrepare, model.TaskASR, model.TaskVision, model.TaskRAGIndex, model.TaskSummary}, nil
	default:
		return nil, fmt.Errorf("unknown upload method %q", uploadMethod)
	}
}

// PlaceholderProcessor 保证尚未接入的阶段仍能推进；真实阶段应实现可取消的 context 调用。
type PlaceholderProcessor struct{}

func (PlaceholderProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		log.Printf("received %s job %s for video %s", job.TaskType, job.JobID, job.VideoID)
		return nil
	}
}
