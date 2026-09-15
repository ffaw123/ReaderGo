package visionagent

import (
	"context"
	"encoding/json"
	"fmt"

	"readergo/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type repository struct {
	db *gorm.DB
}

// dataRepository 隔离 Agent 状态机与数据库实现，便于验证循环预算和终止条件。
type dataRepository interface {
	loadVideo(context.Context, string, string) (model.Video, error)
	loadWindows(context.Context, string, string, Range) ([]RetrievalWindow, error)
	loadMemories(context.Context, Request) ([]Memory, error)
}

func (r *repository) loadVideo(ctx context.Context, tenantID, videoID string) (model.Video, error) {
	var video model.Video
	if err := r.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", videoID, tenantID).First(&video).Error; err != nil {
		return video, fmt.Errorf("load vision video: %w", err)
	}
	if video.Status != model.VideoStatusReady || video.ObjectKey == "" || video.DurationMS <= 0 {
		return video, ErrNotReady
	}
	return video, nil
}

func (r *repository) loadWindows(ctx context.Context, tenantID, videoID string, value Range) ([]RetrievalWindow, error) {
	var segments []model.VideoSegment
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND video_id = ? AND start_ms < ? AND end_ms > ?", tenantID, videoID, value.EndMS, value.StartMS).
		Order("start_ms ASC, end_ms ASC, id ASC").Find(&segments).Error; err != nil {
		return nil, fmt.Errorf("load vision retrieval windows: %w", err)
	}
	if len(segments) == 0 {
		return nil, ErrNotReady
	}
	windows := make([]RetrievalWindow, len(segments))
	for index, segment := range segments {
		windows[index] = RetrievalWindow{SegmentID: segment.ID, StartMS: segment.StartMS, EndMS: segment.EndMS, RetrievalText: segment.RetrievalText}
	}
	return windows, nil
}

func (r *repository) loadMemories(ctx context.Context, request Request) ([]Memory, error) {
	var rows []model.VisionObservation
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND video_id = ? AND conversation_id = ?", request.TenantID, request.VideoID, request.ConversationID).
		Order(clause.Expr{SQL: "CASE WHEN start_ms < ? AND end_ms > ? THEN 0 ELSE 1 END", Vars: []any{request.Range.EndMS, request.Range.StartMS}}).
		Order("created_at DESC").
		Limit(MaxMemoryTurns * 2).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load vision memories: %w", err)
	}
	memories := make([]Memory, 0, len(rows))
	for _, row := range rows {
		var timestamps []int64
		if err := json.Unmarshal(row.UsedFrameTimestamps, &timestamps); err != nil {
			return nil, fmt.Errorf("decode vision memory timestamps: %w", err)
		}
		memories = append(memories, Memory{
			StartMS: row.StartMS, EndMS: row.EndMS, Status: row.Status,
			Observation: row.ObservationText, UsedFrameTimestamps: timestamps,
		})
	}
	return trimMemories(memories), nil
}
