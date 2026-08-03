package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"readergo/internal/model"
	"readergo/internal/outbox"
	"readergo/internal/queue"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const pauseStatePollInterval = 750 * time.Millisecond

type Processor interface {
	Process(context.Context, queue.VideoJobMessage) error
}

type Options struct {
	WorkerID          string
	MaxAttempts       int
	RetryDelays       []time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
}

// Consumer 只处理业务状态；RabbitMQ delivery tag 和 ACK 始终留在 queue 消费层。
type Consumer struct {
	db        *gorm.DB
	processor Processor
	outbox    *outbox.Writer
	options   Options
}

func New(db *gorm.DB, processor Processor, writer *outbox.Writer, options Options) *Consumer {
	if options.WorkerID == "" {
		options.WorkerID = uuid.NewString()
	}
	return &Consumer{db: db, processor: processor, outbox: writer, options: options}
}

// Handle 在终态或下一次可靠通知已经提交后返回 nil，RabbitMQ 层此时才会 ACK。
func (x *Consumer) Handle(ctx context.Context, message queue.VideoJobMessage) error {
	job, claimed, err := x.claimJob(ctx, message)
	if err != nil || !claimed {
		return err
	}

	processorCtx, stopMonitor, pauseRequested, leaseLost := x.withLifecycleMonitor(ctx, job)
	processErr := x.processor.Process(processorCtx, message)
	stopMonitor()
	if leaseLost.Load() {
		// 新租约持有者已接管时，旧 Worker 不能再写任务终态。
		return nil
	}
	if processErr != nil {
		if pauseRequested.Load() {
			return x.returnJobToPendingAfterPause(ctx, job)
		}
		if ctx.Err() != nil {
			// 连接或进程退出时保留 running，Reconciler 会在租约过期后可靠恢复。
			return ctx.Err()
		}
		if err := x.handleProcessFailure(ctx, job, message.UploadMethod, processErr.Error()); err != nil {
			return err
		}
		log.Printf("job %s attempt %d failed and was persisted for retry or DLQ: %v", job.ID, job.Attempt, processErr)
		return nil
	}
	return x.completeJob(ctx, job, message.UploadMethod)
}

// claimJob 用视频行锁和条件更新完成原子抢占，重复消息最多只有一个进入 Processor。
func (x *Consumer) claimJob(ctx context.Context, message queue.VideoJobMessage) (model.ProcessingJob, bool, error) {
	var job model.ProcessingJob
	claimed := false
	if err := queue.ValidateMessage(message); err != nil {
		return job, false, queue.Permanent(err)
	}
	err := x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", message.VideoID, message.TenantID).First(&video).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return queue.Permanent(fmt.Errorf("video %s does not exist", message.VideoID))
			}
			return err
		}
		if video.UploadMethod != message.UploadMethod {
			return queue.Permanent(fmt.Errorf("job upload method %q does not match video %q", message.UploadMethod, video.UploadMethod))
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, "id = ?", message.JobID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return queue.Permanent(fmt.Errorf("job %s does not exist", message.JobID))
			}
			return err
		}
		if job.TaskType != message.TaskType || job.VideoID != message.VideoID || job.TenantID != message.TenantID {
			return queue.Permanent(fmt.Errorf("job message does not match persisted job %s", job.ID))
		}
		// 旧 attempt 与已执行/正在执行的重复通知都直接确认，不允许倒退数据库状态。
		if message.Attempt < job.Attempt || job.Status != model.JobStatusPending || video.Status == model.VideoStatusPaused || video.Status == model.VideoStatusReady || video.Status == model.VideoStatusFailed {
			return nil
		}
		if message.Attempt > job.Attempt {
			return queue.Permanent(fmt.Errorf("message attempt %d is ahead of persisted attempt %d", message.Attempt, job.Attempt))
		}
		ready, err := x.prerequisitesSatisfied(tx, &job, message.UploadMethod)
		if err != nil {
			return err
		}
		if !ready {
			return queue.Permanent(fmt.Errorf("prerequisites are not complete for %s job %s", job.TaskType, job.ID))
		}
		leaseOwner := x.options.WorkerID + ":" + uuid.NewString()
		leaseExpiresAt := time.Now().UTC().Add(x.options.LeaseTTL)
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ? AND attempt = ?", job.ID, model.JobStatusPending, job.Attempt).
			Updates(map[string]any{"status": model.JobStatusRunning, "error_message": "", "lease_owner": leaseOwner, "lease_expires_at": leaseExpiresAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		job.Status, job.LeaseOwner, job.LeaseExpiresAt = model.JobStatusRunning, leaseOwner, &leaseExpiresAt
		claimed = true
		if job.TaskType == model.TaskPrepare {
			return nil
		}
		return tx.Model(&video).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
	})
	return job, claimed, err
}

func (x *Consumer) withLifecycleMonitor(parent context.Context, job model.ProcessingJob) (context.Context, func(), *atomic.Bool, *atomic.Bool) {
	processorCtx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	pauseRequested, leaseLost := &atomic.Bool{}, &atomic.Bool{}
	go func() {
		defer close(stopped)
		pauseTicker := time.NewTicker(pauseStatePollInterval)
		heartbeatTicker := time.NewTicker(x.options.HeartbeatInterval)
		defer pauseTicker.Stop()
		defer heartbeatTicker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-parent.Done():
				return
			case <-pauseTicker.C:
				var video model.Video
				if err := x.db.WithContext(processorCtx).Select("status").First(&video, "id = ?", job.VideoID).Error; err == nil && video.Status == model.VideoStatusPaused {
					pauseRequested.Store(true)
					cancel()
					return
				}
			case <-heartbeatTicker.C:
				result := x.db.WithContext(processorCtx).Model(&model.ProcessingJob{}).
					Where("id = ? AND status = ? AND lease_owner = ?", job.ID, model.JobStatusRunning, job.LeaseOwner).
					Update("lease_expires_at", time.Now().UTC().Add(x.options.LeaseTTL))
				if result.Error != nil {
					log.Printf("renew lease for job %s: %v", job.ID, result.Error)
					continue
				}
				if result.RowsAffected != 1 {
					leaseLost.Store(true)
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
	return processorCtx, stopMonitor, pauseRequested, leaseLost
}

func (x *Consumer) returnJobToPendingAfterPause(ctx context.Context, job model.ProcessingJob) error {
	return x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ? AND lease_owner = ?", job.ID, model.JobStatusRunning, job.LeaseOwner).
			Updates(map[string]any{"status": model.JobStatusPending, "error_message": "", "lease_owner": "", "lease_expires_at": nil})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("paused job %s lost its lease", job.ID)
		}
		return nil
	})
}

func (x *Consumer) completeJob(ctx context.Context, job model.ProcessingJob, uploadMethod string) error {
	return x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		result := tx.Model(&model.ProcessingJob{}).
			Where("id = ? AND status = ? AND lease_owner = ?", job.ID, model.JobStatusRunning, job.LeaseOwner).
			Updates(map[string]any{"status": model.JobStatusSucceeded, "error_message": "", "lease_owner": "", "lease_expires_at": nil})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("job %s lost its lease before completion", job.ID)
		}
		next, err := x.createNextJobs(tx, job)
		if err != nil {
			return err
		}
		finalStatus, err := x.reconcileVideoStatusLocked(tx, &video, uploadMethod)
		if err != nil {
			return err
		}
		if finalStatus != model.VideoStatusPaused {
			for _, nextJob := range next {
				if err := x.outbox.EnqueueJob(tx, nextJob, uploadMethod, time.Now().UTC()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (x *Consumer) handleProcessFailure(ctx context.Context, job model.ProcessingJob, uploadMethod, errorMessage string) error {
	return x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var video model.Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
			return err
		}
		var current model.ProcessingJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", job.ID).Error; err != nil {
			return err
		}
		if current.Status != model.JobStatusRunning || current.LeaseOwner != job.LeaseOwner {
			return nil
		}
		if video.Status == model.VideoStatusPaused {
			// 暂停边界恰好与 Processor 失败重合时不消耗 attempt，恢复接口会重新派发当前任务。
			return tx.Model(&current).Updates(map[string]any{
				"status": model.JobStatusPending, "error_message": "", "lease_owner": "", "lease_expires_at": nil,
			}).Error
		}
		if current.Attempt < x.options.MaxAttempts {
			current.Attempt++
			if err := tx.Model(&current).Updates(map[string]any{
				"status": model.JobStatusPending, "attempt": current.Attempt, "error_message": errorMessage,
				"lease_owner": "", "lease_expires_at": nil,
			}).Error; err != nil {
				return err
			}
			return x.outbox.EnqueueJob(tx, current, uploadMethod, time.Now().UTC().Add(x.retryDelay(current.Attempt-1)))
		}
		if err := tx.Model(&current).Updates(map[string]any{
			"status": model.JobStatusFailed, "error_message": errorMessage, "lease_owner": "", "lease_expires_at": nil,
		}).Error; err != nil {
			return err
		}
		if _, err := x.reconcileVideoStatusLocked(tx, &video, uploadMethod); err != nil {
			return err
		}
		return x.outbox.EnqueueDeadLetter(tx, current, uploadMethod)
	})
}

func (x *Consumer) retryDelay(failedAttempt int) time.Duration {
	index := failedAttempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(x.options.RetryDelays) {
		index = len(x.options.RetryDelays) - 1
	}
	return x.options.RetryDelays[index]
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
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "video_id"}, {Name: "task_type"}}, DoNothing: true}).Create(&candidate)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			next = append(next, candidate)
		}
	}
	return next, nil
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
	video.Status, video.ErrorMessage = status, errorMessage
	return status, nil
}

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

// ReconcileExpired 回收崩溃 Worker 遗留的过期 running 任务，并在同一事务追加新 Outbox。
func (x *Consumer) ReconcileExpired(ctx context.Context) (int, error) {
	var ids []string
	if err := x.db.WithContext(ctx).Model(&model.ProcessingJob{}).
		Where("status = ? AND lease_expires_at < ?", model.JobStatusRunning, time.Now().UTC()).
		Order("lease_expires_at").Limit(100).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	recovered := 0
	for _, id := range ids {
		err := x.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var job model.ProcessingJob
			if err := tx.First(&job, "id = ?", id).Error; err != nil {
				return err
			}
			var video model.Video
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&video, "id = ?", job.VideoID).Error; err != nil {
				return err
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, "id = ?", id).Error; err != nil {
				return err
			}
			if job.Status != model.JobStatusRunning || job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.Before(time.Now().UTC()) {
				return nil
			}
			if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusPending, "lease_owner": "", "lease_expires_at": nil}).Error; err != nil {
				return err
			}
			if video.Status != model.VideoStatusPaused && video.Status != model.VideoStatusReady && video.Status != model.VideoStatusFailed {
				if err := x.outbox.EnqueueJob(tx, job, video.UploadMethod, time.Now().UTC()); err != nil {
					return err
				}
			}
			recovered++
			return nil
		})
		if err != nil {
			return recovered, err
		}
	}
	return recovered, nil
}

func (x *Consumer) RunReconciler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := x.ReconcileExpired(ctx)
			if err != nil {
				log.Printf("reconcile expired job leases: %v", err)
			} else if count > 0 {
				log.Printf("recovered %d expired job leases", count)
			}
		}
	}
}

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
