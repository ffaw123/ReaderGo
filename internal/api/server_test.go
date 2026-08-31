package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"readergo/internal/model"
	"readergo/internal/storage"
	"readergo/internal/uploadratelimit"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
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

func TestValidLowerSHA256(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !validLowerSHA256(valid) {
		t.Fatal("valid lowercase SHA-256 was rejected")
	}
	for _, value := range []string{valid[:63], strings.ToUpper(valid), strings.Repeat("g", 64)} {
		if validLowerSHA256(value) {
			t.Fatalf("invalid SHA-256 %q was accepted", value)
		}
	}
}

func TestIsUploadDedupConflictOnlyMatchesExpectedConstraint(t *testing.T) {
	dedupErr := &pgconn.PgError{Code: "23505", ConstraintName: uploadDedupConstraint}
	if !isUploadDedupConflict(dedupErr) {
		t.Fatal("dedup unique conflict was not recognized")
	}
	otherErr := &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}
	if isUploadDedupConflict(otherErr) {
		t.Fatal("unrelated unique conflict was treated as upload duplicate")
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

type fakeUploadLimiter struct {
	result uploadratelimit.Result
	err    error
	calls  *int
}

func (l fakeUploadLimiter) Allow(context.Context, string) (uploadratelimit.Result, error) {
	if l.calls != nil {
		*l.calls++
	}
	return l.result, l.err
}

func TestAllowUploadReturnsRateLimitResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		uploadLimiter:       fakeUploadLimiter{result: uploadratelimit.Result{Remaining: 0, RetryAfter: 1500 * time.Millisecond}},
		uploadLimitCapacity: 5,
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/uploads", nil)
	ginContext.Set("userID", "tenant-1")

	if server.allowUpload(ginContext) {
		t.Fatal("limited upload was allowed")
	}
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "2" || recorder.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("rate limit response = (%d, %#v)", recorder.Code, recorder.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "UPLOAD_RATE_LIMITED" || body["retry_after_seconds"] != float64(2) {
		t.Fatalf("rate limit body = %#v", body)
	}
}

func TestAllowUploadFailsClosedWhenRedisIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{uploadLimiter: fakeUploadLimiter{err: errors.New("redis unavailable")}, uploadLimitCapacity: 5}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/uploads", nil)
	ginContext.Set("userID", "tenant-1")

	if server.allowUpload(ginContext) {
		t.Fatal("upload was allowed while limiter was unavailable")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestVideoCreationEndpointsShareUploadLimiter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	calls := 0
	server := &Server{
		uploadLimiter:       fakeUploadLimiter{result: uploadratelimit.Result{Remaining: 0, RetryAfter: time.Minute}, calls: &calls},
		uploadLimitCapacity: 5,
		partSize:            32 * 1024 * 1024,
	}
	tests := []struct {
		name string
		path string
		body string
		run  func(*gin.Context)
	}{
		{
			name: "local upload", path: "/api/uploads",
			body: `{"filename":"video.mp4","content_type":"video/mp4","file_size":1,"file_last_modified_ms":1,"file_fingerprint":"` + strings.Repeat("a", 64) + `","content_sha256":"` + strings.Repeat("b", 64) + `"}`,
			run:  server.createUpload,
		},
		{name: "URL upload", path: "/api/videos", body: `{"source_url":"https://example.com/video"}`, run: server.createVideo},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			ginContext.Request.Header.Set("Content-Type", "application/json")
			ginContext.Set("userID", "tenant-1")
			test.run(ginContext)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if calls != len(tests) {
		t.Fatalf("limiter calls = %d, want %d", calls, len(tests))
	}
}
