package api

import (
	"testing"
	"time"

	"readergo/internal/model"
	"readergo/internal/storage"
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

func TestValidUploadedPartsUsesExpectedLayout(t *testing.T) {
	session := model.UploadSession{PartSize: 16, FileSize: 37, PartCount: 3}
	uploaded := []storage.UploadedPart{
		{PartNumber: 3, ETag: "etag-3", Size: 5},
		{PartNumber: 1, ETag: "etag-1", Size: 16},
		// 大小不正确的分片不能被当作已完成，恢复时前端会覆盖上传该编号。
		{PartNumber: 2, ETag: "etag-2", Size: 15},
		{PartNumber: 4, ETag: "outside-layout", Size: 1},
	}

	parts, uploadedBytes := validUploadedParts(session, uploaded)
	if uploadedBytes != 21 {
		t.Fatalf("uploaded bytes = %d, want 21", uploadedBytes)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 3 {
		t.Fatalf("valid parts = %#v, want part numbers [1 3]", parts)
	}
}

func TestUploadSessionResponseReportsAuthoritativeProgress(t *testing.T) {
	session := model.UploadSession{ID: "upload", VideoID: "video", FileSize: 40, PartSize: 16, PartCount: 3}
	parts := []storage.UploadedPart{{PartNumber: 1, ETag: "etag-1", Size: 16}}

	response := uploadSessionResponse(session, parts, nil)
	if response["uploaded_bytes"] != int64(16) || response["progress"] != 40 {
		t.Fatalf("response progress = (%v, %v), want (16, 40)", response["uploaded_bytes"], response["progress"])
	}
}
