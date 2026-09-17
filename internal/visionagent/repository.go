package visionagent

import (
	"context"
	"fmt"

	"readergo/internal/model"

	"gorm.io/gorm"
)

type repository struct {
	db *gorm.DB
}

// dataRepository 隔离 Agent 状态机与数据库实现，便于验证循环预算和终止条件。
type dataRepository interface {
	loadVideo(context.Context, string, string) (model.Video, error)
	loadWindows(context.Context, string, string, Range) ([]RetrievalWindow, error)
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
