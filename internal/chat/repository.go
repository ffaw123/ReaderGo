package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"readergo/internal/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type repository struct {
	db              *gorm.DB
	generationLease time.Duration
}

type beginMessageResult struct {
	UserMessage      model.Message
	AssistantMessage *model.Message
}

func (r *repository) BeginUserMessage(ctx context.Context, in ReplyInput) (beginMessageResult, error) {
	var result beginMessageResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var conversation model.Conversation
		// 锁住会话行，确保同一会话不会并发读取相同历史并生成两份回答。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ? AND video_id = ?", in.Conversation.ID, in.TenantID, in.Conversation.VideoID).
			First(&conversation).Error; err != nil {
			return err
		}
		staleBefore := time.Now().UTC().Add(-r.generationLease)
		// API 异常退出可能遗留 generating；租约过期后允许安全重试，避免会话永久锁死。
		if err := tx.Model(&model.Message{}).
			Where("conversation_id = ? AND role = ? AND generation_status = ? AND generation_started_at < ?", conversation.ID, "user", "generating", staleBefore).
			Update("generation_status", "failed").Error; err != nil {
			return err
		}

		var existing model.Message
		err := tx.Where("conversation_id = ? AND client_message_id = ?", conversation.ID, in.ClientMessageID).First(&existing).Error
		if err == nil {
			result.UserMessage = existing
			switch existing.GenerationStatus {
			case "completed":
				var assistant model.Message
				if err := tx.Where("reply_to_message_id = ? AND role = ?", existing.ID, "assistant").First(&assistant).Error; err != nil {
					return fmt.Errorf("load idempotent assistant message: %w", err)
				}
				result.AssistantMessage = &assistant
				return nil
			case "generating":
				return ErrConversationBusy
			case "failed":
				return tx.Model(&existing).Updates(map[string]any{"generation_status": "generating", "generation_started_at": time.Now().UTC()}).Error
			default:
				return fmt.Errorf("unknown message generation status %q", existing.GenerationStatus)
			}
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var generating int64
		if err := tx.Model(&model.Message{}).
			Where("conversation_id = ? AND role = ? AND generation_status = ?", conversation.ID, "user", "generating").
			Count(&generating).Error; err != nil {
			return err
		}
		if generating > 0 {
			return ErrConversationBusy
		}

		clientMessageID := in.ClientMessageID
		message := model.Message{
			ID: uuid.NewString(), ConversationID: conversation.ID, Role: "user", Content: in.Content,
			Citations: model.NewJSONDocument([]Citation{}), Metadata: model.NewJSONDocument(map[string]any{}),
			ClientMessageID: &clientMessageID, GenerationStatus: "generating", GenerationStartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		}
		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		result.UserMessage = message
		return nil
	})
	return result, err
}

func (r *repository) MarkUserMessageFailed(ctx context.Context, messageID string) {
	_ = r.db.WithContext(ctx).Model(&model.Message{}).
		Where("id = ? AND generation_status = ?", messageID, "generating").
		Update("generation_status", "failed").Error
}

func (r *repository) CompleteReply(ctx context.Context, user model.Message, reply generatedReply) (model.Message, error) {
	metadata := MessageMetadata{
		Mode: reply.Route.Mode, Intent: reply.Route.Intent, IntentConfidence: reply.Decision.Confidence,
		AmbiguityReason: reply.Decision.AmbiguityReason, Notice: reply.Route.Notice, Action: reply.Route.Action,
		SummaryState: reply.Capabilities.Summary, RAGState: reply.Capabilities.RAG,
		IntentPromptVersion: reply.IntentPromptVersion, AnswerPromptVersion: reply.AnswerPromptVersion,
		LatencyMS: durationMilliseconds(reply.Latency),
	}
	userID := user.ID
	assistant := model.Message{
		ID: uuid.NewString(), ConversationID: user.ConversationID, Role: "assistant", Content: reply.Content,
		Citations: model.NewJSONDocument(reply.Citations), Metadata: model.NewJSONDocument(metadata),
		ReplyToMessageID: &userID, GenerationStatus: "completed", GenerationStartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&assistant).Error; err != nil {
			return err
		}
		result := tx.Model(&model.Message{}).
			Where("id = ? AND generation_status = ?", user.ID, "generating").
			Update("generation_status", "completed")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("user message is no longer generating")
		}
		return tx.Model(&model.Conversation{}).Where("id = ?", user.ConversationID).
			Update("updated_at", assistant.CreatedAt).Error
	})
	return assistant, err
}

func durationMilliseconds(values map[string]time.Duration) map[string]int {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]int, len(values))
	for key, value := range values {
		result[key] = int(value.Milliseconds())
	}
	return result
}

func (r *repository) LoadHistory(ctx context.Context, conversationID string, limit int) ([]HistoryTurn, error) {
	var rows []struct {
		UserContent      string
		AssistantContent string
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT u.content AS user_content, a.content AS assistant_content
		FROM messages AS a
		JOIN messages AS u ON u.id = a.reply_to_message_id
		WHERE a.conversation_id = ? AND a.role = 'assistant'
		  AND u.role = 'user' AND u.generation_status = 'completed'
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT ?`, conversationID, limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	turns := make([]HistoryTurn, len(rows))
	for index := range rows {
		row := rows[len(rows)-1-index]
		turns[index] = HistoryTurn{User: row.UserContent, Assistant: row.AssistantContent}
	}
	return turns, nil
}

func (r *repository) LoadCapabilities(ctx context.Context, tenantID, videoID string) (Capabilities, error) {
	capabilities := Capabilities{SummaryState: CapabilityStateProcessing, RAGState: CapabilityStateProcessing}
	var video model.Video
	if err := r.db.WithContext(ctx).Select("title").Where("id = ? AND tenant_id = ?", videoID, tenantID).First(&video).Error; err != nil {
		return capabilities, err
	}
	capabilities.VideoTitle = video.Title
	var jobs []model.ProcessingJob
	if err := r.db.WithContext(ctx).Select("task_type", "status", "error_message").
		Where("tenant_id = ? AND video_id = ? AND task_type IN ?", tenantID, videoID, []string{model.TaskRAGIndex, model.TaskEmbedding, model.TaskSummary}).
		Find(&jobs).Error; err != nil {
		return capabilities, err
	}
	jobByType := make(map[string]model.ProcessingJob, len(jobs))
	for _, job := range jobs {
		jobByType[job.TaskType] = job
	}

	if job, ok := jobByType[model.TaskSummary]; ok && job.Status == model.JobStatusFailed {
		capabilities.SummaryState, capabilities.SummaryError = CapabilityStateFailed, job.ErrorMessage
	} else if ok && job.Status == model.JobStatusSucceeded {
		var summary model.VideoSummary
		if err := r.db.WithContext(ctx).Where("video_id = ?", videoID).First(&summary).Error; err == nil {
			capabilities.Summary = parseSummaryContext(summary)
			if capabilities.Summary.Overview != "" && len(capabilities.Summary.ChapterTitles) > 0 {
				capabilities.SummaryState = CapabilityStateReady
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return capabilities, err
		}
	}

	if job, ok := jobByType[model.TaskRAGIndex]; ok && job.Status == model.JobStatusFailed {
		capabilities.RAGState, capabilities.RAGError = CapabilityStateFailed, job.ErrorMessage
		return capabilities, nil
	}
	if job, ok := jobByType[model.TaskEmbedding]; ok && job.Status == model.JobStatusFailed {
		capabilities.RAGState, capabilities.RAGError = CapabilityStateFailed, job.ErrorMessage
		return capabilities, nil
	}
	if jobByType[model.TaskRAGIndex].Status != model.JobStatusSucceeded || jobByType[model.TaskEmbedding].Status != model.JobStatusSucceeded {
		return capabilities, nil
	}

	var segmentCount, embeddingCount, sourceCount int64
	if err := r.db.WithContext(ctx).Model(&model.VideoSegment{}).
		Where("tenant_id = ? AND video_id = ?", tenantID, videoID).Count(&segmentCount).Error; err != nil {
		return capabilities, err
	}
	if err := r.db.WithContext(ctx).Model(&model.SegmentEmbedding{}).
		Where("tenant_id = ? AND video_id = ?", tenantID, videoID).Count(&embeddingCount).Error; err != nil {
		return capabilities, err
	}
	var asrCount, ocrCount int64
	if err := r.db.WithContext(ctx).Model(&model.ASRSegment{}).
		Where("tenant_id = ? AND video_id = ?", tenantID, videoID).Count(&asrCount).Error; err != nil {
		return capabilities, err
	}
	if err := r.db.WithContext(ctx).Model(&model.OCRSegment{}).
		Where("tenant_id = ? AND video_id = ?", tenantID, videoID).Count(&ocrCount).Error; err != nil {
		return capabilities, err
	}
	sourceCount = asrCount + ocrCount
	if segmentCount > 0 && embeddingCount == segmentCount && sourceCount > 0 {
		capabilities.RAGState = CapabilityStateReady
	} else {
		capabilities.RAGState = CapabilityStateFailed
		capabilities.RAGError = "检索任务已结束，但索引或原始证据不完整"
	}
	return capabilities, nil
}

func parseSummaryContext(summary model.VideoSummary) SummaryContext {
	context := SummaryContext{Overview: strings.TrimSpace(summary.Overview)}
	var chapters []struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(summary.Chapters, &chapters) == nil {
		for _, chapter := range chapters {
			if title := strings.TrimSpace(chapter.Title); title != "" {
				context.ChapterTitles = append(context.ChapterTitles, title)
			}
		}
	}
	if context.Overview == "" || len(context.ChapterTitles) == 0 {
		// 仅兼容迁移前已生成的 Markdown；新摘要必须使用结构化字段。
		legacy := parseLegacySummaryMarkdown(summary.Content)
		if context.Overview == "" {
			context.Overview = legacy.Overview
		}
		if len(context.ChapterTitles) == 0 {
			context.ChapterTitles = legacy.ChapterTitles
		}
	}
	return context
}

var legacyChapterPattern = regexp.MustCompile(`^###\s+(?:\d+\.\s*)?(.+?)\s*$`)

func parseLegacySummaryMarkdown(content string) SummaryContext {
	var result SummaryContext
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	inOverview := false
	var overview []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "## 总体概览" {
			inOverview = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") && trimmed != "## 总体概览" {
			inOverview = false
		}
		if match := legacyChapterPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			result.ChapterTitles = append(result.ChapterTitles, strings.TrimSpace(match[1]))
			inOverview = false
			continue
		}
		if inOverview && trimmed != "" {
			overview = append(overview, trimmed)
		}
	}
	result.Overview = strings.Join(overview, "\n")
	return result
}

func replyResultFromExisting(user model.Message, assistant model.Message) (ReplyResult, error) {
	var metadata MessageMetadata
	if err := json.Unmarshal(assistant.Metadata, &metadata); err != nil {
		return ReplyResult{}, fmt.Errorf("decode existing message metadata: %w", err)
	}
	return ReplyResult{
		UserMessage: user, AssistantMessage: assistant,
		Route:        RouteInfo{Mode: metadata.Mode, Intent: metadata.Intent, Notice: metadata.Notice, Action: metadata.Action},
		Capabilities: CapabilityView{Summary: metadata.SummaryState, RAG: metadata.RAGState},
	}, nil
}
