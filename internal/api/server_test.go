package api

import (
	"testing"
	"time"

	"readergo/internal/model"
)

func TestProcessingDurationMS(t *testing.T) {
	startedAt := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	jobs := []model.ProcessingJob{
		{CreatedAt: startedAt, UpdatedAt: startedAt.Add(time.Minute)},
		{CreatedAt: startedAt.Add(time.Minute), UpdatedAt: startedAt.Add(5 * time.Minute)},
	}

	if got := processingDurationMS(model.VideoStatusProcessing, jobs, startedAt.Add(8*time.Minute)); got != (8 * time.Minute).Milliseconds() {
		t.Fatalf("processing duration = %d, want %d", got, (8 * time.Minute).Milliseconds())
	}
	if got := processingDurationMS(model.VideoStatusReady, jobs, startedAt.Add(8*time.Minute)); got != (5 * time.Minute).Milliseconds() {
		t.Fatalf("ready duration = %d, want %d", got, (5 * time.Minute).Milliseconds())
	}
	if got := processingDurationMS(model.VideoStatusPaused, jobs, startedAt.Add(8*time.Minute)); got != (5 * time.Minute).Milliseconds() {
		t.Fatalf("paused duration = %d, want %d", got, (5 * time.Minute).Milliseconds())
	}
	if got := processingDurationMS(model.VideoStatusReady, nil, startedAt); got != 0 {
		t.Fatalf("empty jobs duration = %d, want 0", got)
	}
}
