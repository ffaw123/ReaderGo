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

// Processor 隔离每个视频处理阶段的具体实现。
// 不同阶段（prepare / asr / vision / rag_index / summary）可分别实现此接口，
// 使消费者循环与具体处理逻辑解耦，便于独立测试和替换。
type Processor interface {
	Process(context.Context, queue.VideoJobMessage) error
}

// Consumer 协调任务执行，数据库是唯一的状态来源（source of truth）。
// 持有 Processor 完成具体处理，持有 Producer 将下一阶段任务写回 Kafka。
type Consumer struct {
	db        *gorm.DB
	processor Processor
	jobs      *queue.Producer
}

// New 创建消费者实例，绑定数据库、处理逻辑和 Kafka 生产者。
func New(db *gorm.DB, processor Processor, jobs *queue.Producer) *Consumer {
	return &Consumer{db: db, processor: processor, jobs: jobs}
}

// Setup 在 Consumer Group 重平衡后、开始消费前调用，用于初始化资源。
func (x *Consumer) Setup(sarama.ConsumerGroupSession) error { return nil }

// Cleanup 在 Consumer Group 重平衡后、停止消费时调用，用于释放资源。
func (x *Consumer) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim 是 Sarama ConsumerGroupHandler 的核心方法。
// 每个分区对应一个独立的 goroutine 调用本方法，循环读取该分区的消息。
// 无论处理成功还是失败都提交位点（MarkMessage），避免单条坏消息阻塞整个分区。
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
		// 无论成功或失败都提交位点，防止坏消息卡死消费
		session.MarkMessage(message, "")
	}
	return nil
}

// run 是任务处理的核心流程，执行以下步骤：
//  1. 从数据库加载任务，校验幂等性（已完成则跳过）
//  2. 检查前置依赖是否满足（如 ASR 必须在 prepare 之后）
//  3. 更新状态为 running 并调用 Processor 执行
//  4. 成功后创建下一阶段任务（如 prepare → asr + vision）
//  5. 将下一阶段任务发布回 Kafka，驱动流水线继续推进
func (x *Consumer) run(ctx context.Context, message queue.VideoJobMessage) error {
	// 步骤1：从数据库加载持久化的任务记录，以数据库为准（message 仅作通知）
	var job model.ProcessingJob
	if err := x.db.First(&job, "id = ?", message.JobID).Error; err != nil {
		return err
	}
	// 幂等性保护：已完成的任务直接返回，避免重复执行
	if job.Status == model.JobStatusSucceeded {
		return nil
	}
	// 校验消息体与数据库记录一致，防止消息串位
	if job.TaskType != message.TaskType || job.VideoID != message.VideoID {
		return fmt.Errorf("job message does not match persisted job %s", job.ID)
	}
	if !queue.IsUploadMethod(message.UploadMethod) {
		return fmt.Errorf("job message has unknown upload method %q", message.UploadMethod)
	}
	// 步骤2：检查前置依赖，依赖未满足则本次处理失败、等待重试
	if ready, err := x.prerequisitesSatisfied(&job, message.UploadMethod); err != nil {
		return err
	} else if !ready {
		return fmt.Errorf("prerequisites are not complete for %s job %s", job.TaskType, job.ID)
	}
	// 步骤3：将任务与视频状态原子更新为 running
	if err := x.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusRunning, "error_message": ""}).Error; err != nil {
			return err
		}
		// url下载文件任务时，文件还未下载，不做视频状态更新
		if job.TaskType == model.TaskPrepare {
			return nil
		}
		return tx.Model(&model.Video{}).Where("id = ?", job.VideoID).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
	}); err != nil {
		return err
	}
	// 调用具体处理器（ASR / Vision / RAG 等）
	if err := x.processor.Process(ctx, message); err != nil {
		_ = x.markJobFailed(job, err.Error(), message.UploadMethod)
		return err
	}

	// 步骤4 & 5：标记当前任务成功，创建并发布下一阶段任务
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
		return x.reconcileVideoStatus(tx, job.VideoID, message.UploadMethod)
	}); err != nil {
		return err
	}
	// 将下一阶段任务发布到 Kafka，驱动流水线接力
	for _, nextJob := range next {
		if err := x.jobs.Publish(ctx, queue.VideoJobMessage{JobID: nextJob.ID, TenantID: nextJob.TenantID, VideoID: nextJob.VideoID, TaskType: nextJob.TaskType, Attempt: nextJob.Attempt, UploadMethod: message.UploadMethod}); err != nil {
			_ = x.markJobFailed(nextJob, err.Error(), message.UploadMethod)
			return err
		}
	}
	return nil
}

// prerequisitesSatisfied 检查指定任务的所有前置依赖是否已成功完成。
// URL 上传：prepare 无依赖 → ASR/Vision 依赖 prepare → RAG/Summary 依赖 ASR+Vision。
// 本地直传：对象已在 MinIO，ASR/Vision 无需 prepare，直接依赖完整对象。
func (x *Consumer) prerequisitesSatisfied(job *model.ProcessingJob, uploadMethod string) (bool, error) {
	var prerequisiteTypes []string
	switch job.TaskType {
	case model.TaskPrepare:
		// prepare 是流水线入口，无前置依赖
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
	// 统计同视频下、指定类型的前置任务中已成功的数量
	var count int64
	err := x.db.Model(&model.ProcessingJob{}).
		Where("video_id = ? AND task_type IN ? AND status = ?", job.VideoID, prerequisiteTypes, model.JobStatusSucceeded).
		Count(&count).Error
	// 所有前置任务都成功才算满足
	return count == int64(len(prerequisiteTypes)), err
}

// createNextJobs 根据已完成的任务类型，决定流水线的下一步。
// prepare 完成 → 创建 ASR + Vision
// ASR 和 Vision 都完成 → 创建 RAGIndex + Summary
// 使用 ON CONFLICT DO NOTHING 防止重复创建（（video_id, task_type）为唯一约束）
func (x *Consumer) createNextJobs(tx *gorm.DB, completed model.ProcessingJob) ([]model.ProcessingJob, error) {
	var taskTypes []string
	switch completed.TaskType {
	case model.TaskPrepare:
		// prepare 完成后，并行启动 ASR 和 Vision
		taskTypes = []string{model.TaskASR, model.TaskVision}
	case model.TaskASR, model.TaskVision:
		// ASR 和 Vision 都成功后才启动 RAGIndex 和 Summary
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
		// ON CONFLICT DO NOTHING：如已有相同（video_id, task_type）记录则跳过，保证幂等
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "video_id"}, {Name: "task_type"}},
			DoNothing: true,
		}).Create(&candidate)
		if result.Error != nil {
			return nil, result.Error
		}
		// RowsAffected == 1 表示新创建，需要发布到 Kafka；
		// RowsAffected == 0 表示已存在（可能之前重试过），无需重复发布
		if result.RowsAffected == 1 {
			next = append(next, candidate)
		}
	}
	return next, nil
}

// reconcileVideoStatus 是唯一负责从任务状态推导视频整体状态的地方。
// 所有任务完成 → video.status = ready
// 任一任务失败 → video.status = failed
// 其他情况     → video.status = processing（处理中）
func (x *Consumer) reconcileVideoStatus(tx *gorm.DB, videoID, uploadMethod string) error {
	requiredTaskTypes, err := requiredTaskTypesForUploadMethod(uploadMethod)
	if err != nil {
		return err
	}
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
	for _, taskType := range requiredTaskTypes {
		if statuses[taskType] != model.JobStatusSucceeded {
			return tx.Model(&model.Video{}).Where("id = ?", videoID).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error
		}
	}
	return tx.Model(&model.Video{}).Where("id = ?", videoID).Updates(map[string]any{"status": model.VideoStatusReady, "error_message": ""}).Error
}

// markJobFailed 将任务标记为失败，并同步更新视频状态。
func (x *Consumer) markJobFailed(job model.ProcessingJob, errorMessage, uploadMethod string) error {
	return x.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&job).Updates(map[string]any{"status": model.JobStatusFailed, "error_message": errorMessage}).Error; err != nil {
			return err
		}
		return x.reconcileVideoStatus(tx, job.VideoID, uploadMethod)
	})
}

// requiredTaskTypesForUploadMethod 定义进入 ready 所需的阶段，不能把本地直传误判为缺少 prepare。
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

// PlaceholderProcessor 是一个占位处理器，保证在接入具体的媒体和 AI 处理器之前流水线可以正常运行。
// 实际使用时替换为：ASRProcessor（语音识别）、VisionProcessor（视觉分析）等。
type PlaceholderProcessor struct{}

func (PlaceholderProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	log.Printf("received %s job %s for video %s", job.TaskType, job.JobID, job.VideoID)
	return nil
}
