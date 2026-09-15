package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"readergo/internal/auth"
	chatservice "readergo/internal/chat"
	"readergo/internal/model"
	"readergo/internal/outbox"
	"readergo/internal/queue"
	"readergo/internal/storage"
	"readergo/internal/uploaddedupe"
	"readergo/internal/uploadratelimit"
	"readergo/internal/visionagent"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Server 汇集 HTTP 接口依赖，避免处理器直接依赖全局变量。
type Server struct {
	db                  *gorm.DB
	auth                *auth.Service
	chat                *chatservice.Service
	outbox              *outbox.Writer
	uploads             *storage.MultipartStore
	deduper             *uploaddedupe.Service
	uploadLimiter       uploadLimiter
	uploadLimitCapacity int
	partSize            int64
}

type uploadLimiter interface {
	Allow(ctx context.Context, tenantID string) (uploadratelimit.Result, error)
}

var (
	errVideoNotPausable       = errors.New("video is not in a pausable state")
	errVideoNotPaused         = errors.New("video is not paused")
	errPauseStillSettling     = errors.New("running jobs have not stopped yet")
	errPausedVideoHasFailures = errors.New("paused video contains failed jobs")
	errNoPendingJobs          = errors.New("paused video has no pending jobs")
	errUploadSessionNotActive = errors.New("upload session is not active")
	errUploadPartsIncomplete  = errors.New("upload parts are incomplete")
)

const (
	// 首批 URL 与建议的最大并发数一致，避免在初始化时暴露全部分片授权。
	initialUploadURLCount = 8
	// 补充 URL 时允许略多于单轮并发数，以减少网络往返且限制凭证数量。
	maxPartURLBatch = 16
	// S3 Multipart Upload 协议规定的最大分片数。
	maxMultipartParts = 10000
	// 只把上传内容去重索引冲突转换为业务 409，其他唯一约束错误仍按服务端错误处理。
	uploadDedupConstraint = "videos_tenant_content_sha256_active_idx"
)

// createUploadRequest 仅接收客户端元数据，文件二进制不会经过 Gin 服务。
type createUploadRequest struct {
	Filename           string `json:"filename" binding:"required,max=512"`
	ContentType        string `json:"content_type" binding:"max=255"`
	FileSize           int64  `json:"file_size" binding:"required,gt=0"`
	FileLastModifiedMS int64  `json:"file_last_modified_ms" binding:"required,gt=0"`
	FileFingerprint    string `json:"file_fingerprint" binding:"required,len=64"`
	ContentSHA256      string `json:"content_sha256" binding:"required,len=64"`
	Title              string `json:"title" binding:"max=512"`
}

// uploadPartsRequest 让客户端在上传队列需要新槽位时批量申请 URL。
type uploadPartsRequest struct {
	PartNumbers []int `json:"part_numbers" binding:"required,min=1,max=16"`
}

// uploadPartURL 描述一个仅可上传指定分片、且有短期有效期的直传地址。
type uploadPartURL struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// createUpload 创建数据库上传会话和 MinIO Multipart Upload，但不代理文件字节。
func (s *Server) createUpload(c *gin.Context) {
	var in createUploadRequest
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "valid filename, file metadata and fingerprint are required")
		return
	}
	if !validLowerSHA256(in.FileFingerprint) {
		bad(c, "file_fingerprint must be a lowercase SHA-256 hex string")
		return
	}
	if !validLowerSHA256(in.ContentSHA256) {
		bad(c, "content_sha256 must be a lowercase SHA-256 hex string")
		return
	}
	// 文件大小和固定分片大小决定总数；最后一个分片可小于 partSize。
	partCount := int(math.Ceil(float64(in.FileSize) / float64(s.partSize)))
	// 必须在创建 MinIO 上传前拦截超限文件，否则会得到永远无法完成的会话。
	if partCount > maxMultipartParts {
		bad(c, fmt.Sprintf("file is too large for the configured %d MiB part size", s.partSize/(1024*1024)))
		return
	}
	// tenantID 只能来自已验证 JWT，不能由请求体指定。
	tenantID := auth.UserID(c)
	if !s.allowUpload(c) {
		return
	}
	if s.deduper != nil {
		entry, found, err := s.deduper.Find(c.Request.Context(), tenantID, in.ContentSHA256)
		if err != nil {
			fail(c, err)
			return
		}
		if found {
			respondUploadDuplicate(c, entry.VideoID)
			return
		}
	}
	// 预先生成视频 ID，才能构造包含租户和视频归属的对象路径。
	videoID := uuid.NewString()
	// 使用固定对象名，不信任 filename 的扩展名；上传完成后由 FFprobe 校验真实媒体格式。
	objectKey := fmt.Sprintf("tenants/%s/videos/%s/source/original", tenantID, videoID)
	// 先创建 uploading 视频记录，让后续完成上传流程拥有稳定的资源归属。
	contentSHA256 := in.ContentSHA256
	v := model.Video{ID: videoID, TenantID: tenantID, SourceURL: "local://" + in.Filename, Title: in.Title, Status: model.VideoStatusUploading, ContentSHA256: &contentSHA256, UploadMethod: queue.UploadMethodLocalDirect, ObjectKey: objectKey}
	if e := s.db.Create(&v).Error; e != nil {
		if isUploadDedupConflict(e) && s.deduper != nil {
			entry, found, findErr := s.deduper.Find(c.Request.Context(), tenantID, in.ContentSHA256)
			if findErr == nil && found {
				respondUploadDuplicate(c, entry.VideoID)
				return
			}
			if findErr != nil {
				fail(c, findErr)
				return
			}
		}
		fail(c, e)
		return
	}
	// uploadID 是完成 Multipart Upload 所必需的秘密服务端状态，绝不返回给客户端。
	uploadID, e := s.uploads.Create(c.Request.Context(), objectKey, in.ContentType)
	if e != nil {
		// MinIO 初始化失败时移除刚插入的视频，此时session还未创建，直接删除视频（最好和会话配对）
		_ = s.db.Delete(&v).Error
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not initialize object upload"})
		return
	}
	// 上传会话独立保存分片布局、MinIO uploadID 和过期时间，不能只依赖 videos.status。
	session := model.UploadSession{
		TenantID: tenantID, VideoID: videoID, UploadID: uploadID, ObjectKey: objectKey,
		Filename: in.Filename, FileLastModifiedMS: in.FileLastModifiedMS, FileFingerprint: in.FileFingerprint,
		PartSize: s.partSize, FileSize: in.FileSize, PartCount: partCount,
		Status: "uploading", ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if e = s.db.Create(&session).Error; e != nil {
		// 数据库持久化失败后必须中止 MinIO multipart upload，避免产生孤儿临时分片。
		_ = s.uploads.Abort(c.Request.Context(), objectKey, uploadID)
		_ = s.db.Delete(&v).Error
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create upload session"})
		return
	}
	// 仅返回首批 URL，客户端用 /parts 接口按需续取其余 URL。
	parts, e := s.presignParts(c.Request.Context(), session, firstPartNumbers(partCount))
	if e != nil {
		if cancelErr := s.terminateUpload(c.Request.Context(), session.ID, session.TenantID, "cancelled", "could not issue initial upload URLs"); cancelErr != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not cancel object upload"})
			return
		}
		fail(c, e)
		return
	}
	if s.deduper != nil {
		// 只有客户端已获得可用上传会话后才建立正缓存，避免缓存后续初始化失败的记录。
		s.deduper.Remember(c.Request.Context(), tenantID, in.ContentSHA256, videoID)
	}
	c.JSON(http.StatusCreated, uploadSessionResponse(session, nil, parts))
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isUploadDedupConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == uploadDedupConstraint
}

func respondUploadDuplicate(c *gin.Context, videoID string) {
	c.JSON(http.StatusConflict, gin.H{
		"error": "该视频已提交，无需重复上传", "code": "UPLOAD_DUPLICATE", "video_id": videoID,
	})
}

// uploadPartURLs 按需批量签发 URL；客户端以 4 到 8 路并行 PUT 这些 URL 即可。
func (s *Server) uploadPartURLs(c *gin.Context) {
	var in uploadPartsRequest
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "part_numbers must contain 1 to 16 part numbers")
		return
	}
	// 查询同时限制 tenant_id，防止通过猜测 UUID 获取其他用户的直传 URL。
	var session model.UploadSession
	if e := s.db.Where("id = ? AND tenant_id = ?", c.Param("id"), auth.UserID(c)).First(&session).Error; e != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	// 已完成、取消或过期的会话都不应再获得新的上传授权。
	if session.Status != "uploading" {
		c.JSON(http.StatusConflict, gin.H{"error": "upload session is not active"})
		return
	}
	if time.Now().After(session.ExpiresAt) {
		_ = s.terminateUpload(c.Request.Context(), session.ID, session.TenantID, "expired", "upload session expired")
		c.JSON(http.StatusConflict, gin.H{"error": "upload session has expired"})
		return
	}
	// 分片必须位于会话范围内且不重复，避免无效签名或重复授权。
	seen := make(map[int]struct{}, len(in.PartNumbers))
	for _, partNumber := range in.PartNumbers {
		if partNumber < 1 || partNumber > session.PartCount {
			bad(c, "part number is outside the upload range")
			return
		}
		if _, duplicate := seen[partNumber]; duplicate {
			bad(c, "part_numbers must not contain duplicates")
			return
		}
		seen[partNumber] = struct{}{}
	}
	parts, e := s.presignParts(c.Request.Context(), session, in.PartNumbers)
	if e != nil {
		// 签名服务的瞬时错误不能销毁已有分片；客户端稍后可重新申请 URL。
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not issue upload URLs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"upload_id": session.ID, "parts": parts})
}

// getUploadStatus 每次直接查询 MinIO，客户端内存不参与恢复正确性。
func (s *Server) getUploadStatus(c *gin.Context) {
	var session model.UploadSession
	if err := s.db.Where("id = ? AND tenant_id = ?", c.Param("id"), auth.UserID(c)).First(&session).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	if session.Status != "uploading" {
		c.JSON(http.StatusConflict, gin.H{"error": "upload session is not active"})
		return
	}
	if time.Now().After(session.ExpiresAt) {
		_ = s.terminateUpload(c.Request.Context(), session.ID, session.TenantID, "expired", "upload session expired")
		c.JSON(http.StatusConflict, gin.H{"error": "upload session has expired"})
		return
	}
	parts, err := s.uploads.ListParts(c.Request.Context(), session.ObjectKey, session.UploadID)
	if err != nil {
		if errors.Is(err, storage.ErrMultipartUploadNotFound) {
			size, exists, statErr := s.uploads.ObjectSize(c.Request.Context(), session.ObjectKey)
			if statErr == nil && exists && size == session.FileSize {
				// 对象已完成但数据库尚未收敛时，客户端只需重试完成接口，不能重新上传分片。
				response := uploadSessionResponse(session, nil, nil)
				response["object_completed"] = true
				response["uploaded_bytes"] = session.FileSize
				response["progress"] = 100
				c.JSON(http.StatusOK, response)
				return
			}
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not query uploaded parts"})
		return
	}
	validParts, _ := validUploadedParts(session, parts)
	c.JSON(http.StatusOK, uploadSessionResponse(session, validParts, nil))
}

// cancelUploadSession 只用于用户明确取消；暂停仅中止浏览器 PUT，不能调用此接口。
func (s *Server) cancelUploadSession(c *gin.Context) {
	err := s.terminateUpload(c.Request.Context(), c.Param("id"), auth.UserID(c), "cancelled", "upload cancelled by user")
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		case errors.Is(err, errUploadSessionNotActive):
			c.JSON(http.StatusConflict, gin.H{"error": "upload session is not active"})
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not cancel object upload"})
		}
		return
	}
	c.Status(http.StatusNoContent)
}

// terminateUpload 在数据库行锁内中止 MinIO 会话，保证取消不会与完成操作并发穿透。
func (s *Server) terminateUpload(ctx context.Context, sessionID, tenantID, terminalStatus, reason string) error {
	var contentSHA256 string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.UploadSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", sessionID, tenantID).First(&session).Error; err != nil {
			return err
		}
		var video model.Video
		if err := tx.Select("id", "content_sha256").
			Where("id = ? AND tenant_id = ?", session.VideoID, session.TenantID).First(&video).Error; err != nil {
			return err
		}
		if video.ContentSHA256 != nil {
			contentSHA256 = *video.ContentSHA256
		}
		if session.Status == terminalStatus {
			return nil
		}
		if session.Status != "uploading" {
			return errUploadSessionNotActive
		}
		if err := s.uploads.Abort(ctx, session.ObjectKey, session.UploadID); err != nil {
			return err
		}
		if err := tx.Model(&session).Update("status", terminalStatus).Error; err != nil {
			return err
		}
		return tx.Model(&model.Video{}).
			Where("id = ? AND tenant_id = ?", session.VideoID, session.TenantID).
			Updates(map[string]any{"status": model.VideoStatusFailed, "error_message": reason}).Error
	})
	if err == nil && s.deduper != nil && contentSHA256 != "" {
		// 数据库已允许相同文件重试后，再尽力删除可能存在的 Redis 正缓存。
		s.deduper.Forget(ctx, tenantID, contentSHA256)
	}
	return err
}

// RunUploadCleanup 定期中止过期会话；失败会保留 uploading 状态，下一轮继续重试清理。
func (s *Server) RunUploadCleanup(ctx context.Context, interval time.Duration) {
	cleanup := func() {
		if count, err := s.cleanupExpiredUploads(ctx, 100); err != nil {
			log.Printf("cleanup expired uploads: %v", err)
		} else if count > 0 {
			log.Printf("cleaned up %d expired uploads", count)
		}
	}
	cleanup()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func (s *Server) cleanupExpiredUploads(ctx context.Context, limit int) (int, error) {
	var sessions []model.UploadSession
	if err := s.db.WithContext(ctx).
		Where("status = ? AND expires_at <= ?", "uploading", time.Now()).
		Order("expires_at").Limit(limit).Find(&sessions).Error; err != nil {
		return 0, err
	}
	cleaned := 0
	for _, session := range sessions {
		if err := s.terminateUpload(ctx, session.ID, session.TenantID, "expired", "upload session expired"); err != nil {
			if ctx.Err() != nil {
				return cleaned, ctx.Err()
			}
			log.Printf("cleanup expired upload %s: %v", session.ID, err)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

// completeUpload 将 MinIO 中的全部已上传分片提交为最终对象，并创建视频处理任务。
func (s *Server) completeUpload(c *gin.Context) {
	tenantID := auth.UserID(c)
	alreadyCompleted := false
	var video model.Video
	var firstJob model.ProcessingJob
	err := s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var session model.UploadSession
		// 行锁覆盖查询分片、完成对象和写入任务，重复完成请求只能串行收敛一次。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", c.Param("id"), tenantID).First(&session).Error; err != nil {
			return err
		}
		if session.Status == "completed" {
			alreadyCompleted = true
			if err := tx.Where("id = ? AND tenant_id = ?", session.VideoID, tenantID).First(&video).Error; err != nil {
				return err
			}
			return tx.Where("video_id = ? AND task_type = ?", session.VideoID, model.TaskASR).First(&firstJob).Error
		}
		if session.Status != "uploading" || time.Now().After(session.ExpiresAt) {
			return errUploadSessionNotActive
		}

		uploaded, err := s.uploads.ListParts(c.Request.Context(), session.ObjectKey, session.UploadID)
		if err != nil {
			if !errors.Is(err, storage.ErrMultipartUploadNotFound) {
				return fmt.Errorf("list uploaded parts: %w", err)
			}
			// MinIO 完成成功但数据库事务回滚时 upload ID 已消失；完整最终对象可安全重放数据库提交。
			size, exists, statErr := s.uploads.ObjectSize(c.Request.Context(), session.ObjectKey)
			if statErr != nil {
				return fmt.Errorf("stat completed upload object: %w", statErr)
			}
			if !exists || size != session.FileSize {
				return fmt.Errorf("multipart upload disappeared before completion")
			}
		} else {
			parts, _ := validUploadedParts(session, uploaded)
			if len(parts) != session.PartCount {
				return errUploadPartsIncomplete
			}
			completed := make([]storage.CompletedPart, len(parts))
			for i, part := range parts {
				completed[i] = storage.CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag}
			}
			if err := s.uploads.Complete(c.Request.Context(), session.ObjectKey, session.UploadID, completed); err != nil {
				return fmt.Errorf("complete object upload: %w", err)
			}
		}

		if err := tx.Where("id = ? AND tenant_id = ?", session.VideoID, tenantID).First(&video).Error; err != nil {
			return err
		}
		if err := tx.Model(&session).Update("status", "completed").Error; err != nil {
			return err
		}
		if err := tx.Model(&video).Updates(map[string]any{"status": model.VideoStatusProcessing, "error_message": ""}).Error; err != nil {
			return err
		}
		jobs := []model.ProcessingJob{
			{TenantID: tenantID, VideoID: session.VideoID, TaskType: model.TaskASR, Status: model.JobStatusPending, Attempt: 1},
			{TenantID: tenantID, VideoID: session.VideoID, TaskType: model.TaskVision, Status: model.JobStatusPending, Attempt: 1},
		}
		if err := tx.Create(&jobs).Error; err != nil {
			return err
		}
		firstJob = jobs[0]
		// 两个任务及其通知在同一事务提交，RabbitMQ 暂时不可用也不会造成漏派发。
		for _, job := range jobs {
			if err := s.outbox.EnqueueJob(tx, job, queue.UploadMethodLocalDirect, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		case errors.Is(err, errUploadSessionNotActive):
			c.JSON(http.StatusConflict, gin.H{"error": "upload session is not active"})
		case errors.Is(err, errUploadPartsIncomplete):
			c.JSON(http.StatusConflict, gin.H{"error": "not all upload parts are present in object storage"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "could not complete object upload"})
		}
		return
	}

	video.Status = model.VideoStatusProcessing
	video.ErrorMessage = ""
	status := http.StatusCreated
	if alreadyCompleted {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"video": video, "job_id": firstJob.ID})
}

func validUploadedParts(session model.UploadSession, uploaded []storage.UploadedPart) ([]storage.UploadedPart, int64) {
	byNumber := make([]*storage.UploadedPart, session.PartCount)
	for i := range uploaded {
		part := uploaded[i]
		if part.PartNumber < 1 || part.PartNumber > session.PartCount || part.ETag == "" {
			continue
		}
		expectedSize := session.PartSize
		if part.PartNumber == session.PartCount {
			expectedSize = session.FileSize - int64(session.PartCount-1)*session.PartSize
		}
		if part.Size != expectedSize || byNumber[part.PartNumber-1] != nil {
			continue
		}
		partCopy := part
		byNumber[part.PartNumber-1] = &partCopy
	}
	parts := make([]storage.UploadedPart, 0, len(uploaded))
	var uploadedBytes int64
	for _, part := range byNumber {
		if part == nil {
			continue
		}
		parts = append(parts, *part)
		uploadedBytes += part.Size
	}
	return parts, uploadedBytes
}

func uploadSessionResponse(session model.UploadSession, completed []storage.UploadedPart, signed []uploadPartURL) gin.H {
	var uploadedBytes int64
	for _, part := range completed {
		uploadedBytes += part.Size
	}
	progress := 0
	if session.FileSize > 0 {
		progress = int(math.Round(float64(uploadedBytes) / float64(session.FileSize) * 100))
	}
	response := gin.H{
		"upload_id": session.ID, "video_id": session.VideoID, "status": session.Status,
		"filename": session.Filename, "file_size": session.FileSize,
		"file_last_modified_ms": session.FileLastModifiedMS, "file_fingerprint": session.FileFingerprint,
		"part_size": session.PartSize, "part_count": session.PartCount, "expires_at": session.ExpiresAt,
		"completed_parts": completed, "uploaded_bytes": uploadedBytes, "progress": progress,
	}
	if signed != nil {
		response["parts"] = signed
	}
	return response
}

func (s *Server) presignParts(ctx context.Context, session model.UploadSession, numbers []int) ([]uploadPartURL, error) {
	// 保持请求中的编号顺序，前端可直接将响应映射到自己的上传队列。
	parts := make([]uploadPartURL, 0, len(numbers))
	for _, partNumber := range numbers {
		u, expiresAt, err := s.uploads.PresignedPartURL(ctx, session.ObjectKey, session.UploadID, partNumber)
		if err != nil {
			return nil, err
		}
		parts = append(parts, uploadPartURL{PartNumber: partNumber, URL: u, ExpiresAt: expiresAt})
	}
	return parts, nil
}

func firstPartNumbers(partCount int) []int {
	// 小文件的分片总数可能小于默认的首批 URL 数。
	count := min(partCount, initialUploadURLCount)
	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = i + 1
	}
	return numbers
}

// New 使用数据库、鉴权、事务 Outbox、视频上传、去重和限流组件创建 API 服务。
func New(db *gorm.DB, authService *auth.Service, chatService *chatservice.Service, outboxWriter *outbox.Writer, uploads *storage.MultipartStore, deduper *uploaddedupe.Service, limiter uploadLimiter, uploadLimitCapacity int, partSize int64) *Server {
	return &Server{
		db: db, auth: authService, chat: chatService, outbox: outboxWriter, uploads: uploads,
		deduper: deduper, uploadLimiter: limiter, uploadLimitCapacity: uploadLimitCapacity, partSize: partSize,
	}
}

// Router 注册公开鉴权路由和必须携带 JWT 的业务路由。
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	a := r.Group("/api/auth")
	a.POST("/register", s.register)
	a.POST("/login", s.login)
	// /api 下的业务资源必须先经过 JWT 校验。
	p := r.Group("/api")
	p.Use(s.auth.Middleware())
	p.POST("/videos", s.createVideo)
	p.POST("/uploads", s.createUpload)
	p.GET("/uploads/:id", s.getUploadStatus)
	p.POST("/uploads/:id/parts", s.uploadPartURLs)
	p.POST("/uploads/:id/complete", s.completeUpload)
	p.DELETE("/uploads/:id", s.cancelUploadSession)
	p.GET("/videos", s.listVideos)
	p.GET("/videos/:id", s.getVideo) // 这里的id是指视频id
	p.GET("/videos/:id/status", s.videoStatus)
	p.POST("/videos/:id/pause", s.pauseVideo)
	p.POST("/videos/:id/resume", s.resumeVideo)
	p.GET("/videos/:id/summary", s.summary)
	p.GET("/capabilities", s.capabilities)
	p.POST("/videos/:id/conversations", s.createConversation)
	p.POST("/conversations/:id/messages", s.createMessage) // 这里的id是会话id
	p.GET("/conversations/:id/messages", s.messages)
	return r
}

func (s *Server) capabilities(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"vision": gin.H{"enabled": s.chat.VisionAvailable()}})
}

// credentials 是注册和登录共用的请求体；密码长度在进入业务逻辑前校验。
type credentials struct {
	Email       string `json:"email" binding:"required,email"`
	Password    string `json:"password" binding:"required,min=8,max=128"`
	DisplayName string `json:"display_name"`
}

// register 创建账户、保存 bcrypt 密码哈希，并立即返回访问令牌。
func (s *Server) register(c *gin.Context) {
	var in credentials
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "email and password (minimum 8 characters) are required")
		return
	}
	hash, e := auth.HashPassword(in.Password)
	if e != nil {
		fail(c, e)
		return
	}
	u := model.User{Email: strings.ToLower(in.Email), PasswordHash: hash, DisplayName: in.DisplayName}
	if e = s.db.Create(&u).Error; e != nil {
		// 唯一索引冲突被转换为明确的客户端错误，而非暴露数据库细节。
		if strings.Contains(e.Error(), "duplicate") {
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
			return
		}
		fail(c, e)
		return
	}
	s.respondToken(c, u)
}

// login 无论用户不存在还是密码错误都返回相同信息，减少账号枚举风险。
func (s *Server) login(c *gin.Context) {
	var in credentials
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "email and password are required")
		return
	}
	var u model.User
	if e := s.db.Where("email = ?", strings.ToLower(in.Email)).First(&u).Error; e != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}
	if auth.CheckPassword(u.PasswordHash, in.Password) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}
	s.respondToken(c, u)
}

// respondToken 集中处理注册与登录成功后的 JWT 响应格式。
func (s *Server) respondToken(c *gin.Context, u model.User) {
	t, e := s.auth.Token(u.ID)
	if e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"access_token": t, "token_type": "Bearer", "user": u})
}

// createVideoRequest 定义视频提交时允许客户端提供的字段。
type createVideoRequest struct {
	SourceURL string `json:"source_url" binding:"required,url"`
	Title     string `json:"title"`
}

// URL 方式上传视频时触发该函数
// createVideo 在同一事务持久化视频、prepare 任务和 Outbox 通知。
func (s *Server) createVideo(c *gin.Context) {
	var in createVideoRequest
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "source_url must be a valid URL")
		return
	}
	// API 先拒绝明显非法的协议和凭据；Worker 下载前及每次重定向仍会完整执行 SSRF 校验。
	u, e := url.Parse(in.SourceURL)
	if e != nil || len(in.SourceURL) > 2048 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		bad(c, "only http(s) video URLs are allowed")
		return
	}
	tenant := auth.UserID(c)
	if !s.allowUpload(c) {
		return
	}
	v := model.Video{TenantID: tenant, SourceURL: in.SourceURL, Title: in.Title, Status: model.VideoStatusUploading, UploadMethod: queue.UploadMethodURL}
	j := model.ProcessingJob{TenantID: tenant, VideoID: v.ID, TaskType: model.TaskPrepare, Status: model.JobStatusPending, Attempt: 1}
	if e = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&v).Error; err != nil {
			return err
		}
		j.VideoID = v.ID
		if err := tx.Create(&j).Error; err != nil {
			return err
		}
		return s.outbox.EnqueueJob(tx, j, queue.UploadMethodURL, time.Now().UTC())
	}); e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"video": v, "job_id": j.ID})
}

// ownedVideo 在查询中同时限定 tenant_id，防止跨用户访问猜测到的 UUID。
func (s *Server) ownedVideo(c *gin.Context) (model.Video, bool) {
	var v model.Video
	if e := s.db.Where("id = ? AND tenant_id = ?", c.Param("id"), auth.UserID(c)).First(&v).Error; e != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		return v, false
	}
	return v, true
}

// getVideo 返回当前用户拥有的视频元数据。
func (s *Server) getVideo(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if ok {
		c.JSON(http.StatusOK, v)
	}
}

// listVideos 返回当前用户的全部视频，前端默认展示第一条最新记录。
func (s *Server) listVideos(c *gin.Context) {
	tenantID := auth.UserID(c)
	items := make([]model.Video, 0)
	err := s.db.WithContext(c.Request.Context()).
		Where("tenant_id = ?", tenantID).
		Order("created_at DESC").
		Find(&items).Error
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// pauseVideo 只改变视频的期望运行状态；正在执行的任务由 Worker 确认取消后回到 pending。
// API 不能直接把 running 改为 pending，否则旧 Worker 尚未退出时可能被恢复请求重复启动。
func (s *Server) pauseVideo(c *gin.Context) {
	tenantID := auth.UserID(c)
	var video model.Video
	var runningJobs int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 与 Worker 的任务抢占使用同一把视频行锁，保证“开始执行”和“请求暂停”有明确先后顺序。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", c.Param("id"), tenantID).
			First(&video).Error; err != nil {
			return err
		}
		if video.Status == model.VideoStatusPaused {
			return tx.Model(&model.ProcessingJob{}).
				Where("video_id = ? AND status = ?", video.ID, model.JobStatusRunning).
				Count(&runningJobs).Error
		}
		if video.Status != model.VideoStatusProcessing && video.Status != model.VideoStatusUploading {
			return errVideoNotPausable
		}
		// 本地 Multipart 上传由浏览器负责，尚未完成时没有后台任务，不能混用视频处理暂停。
		if video.Status == model.VideoStatusUploading && video.UploadMethod != queue.UploadMethodURL {
			return errVideoNotPausable
		}
		var jobCount int64
		if err := tx.Model(&model.ProcessingJob{}).Where("video_id = ?", video.ID).Count(&jobCount).Error; err != nil {
			return err
		}
		if jobCount == 0 {
			return errVideoNotPausable
		}
		pausedFrom := video.Status
		if err := tx.Model(&video).Updates(map[string]any{
			"status":             model.VideoStatusPaused,
			"paused_from_status": pausedFrom,
		}).Error; err != nil {
			return err
		}
		video.Status = model.VideoStatusPaused
		video.PausedFromStatus = &pausedFrom
		return tx.Model(&model.ProcessingJob{}).
			Where("video_id = ? AND status = ?", video.ID, model.JobStatusRunning).
			Count(&runningJobs).Error
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		case errors.Is(err, errVideoNotPausable):
			c.JSON(http.StatusConflict, gin.H{"error": "video is not in a pausable state"})
		default:
			fail(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"video_id":      video.ID,
		"status":        model.VideoStatusPaused,
		"pause_settled": runningJobs == 0,
	})
}

// resumeVideo 恢复暂停前的总体状态，并只重新投递尚未完成的 pending 任务。
func (s *Server) resumeVideo(c *gin.Context) {
	tenantID := auth.UserID(c)
	var video model.Video
	var pendingJobs []model.ProcessingJob
	restoredStatus := ""
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", c.Param("id"), tenantID).
			First(&video).Error; err != nil {
			return err
		}
		if video.Status != model.VideoStatusPaused {
			return errVideoNotPaused
		}
		var runningJobs int64
		if err := tx.Model(&model.ProcessingJob{}).
			Where("video_id = ? AND status = ?", video.ID, model.JobStatusRunning).
			Count(&runningJobs).Error; err != nil {
			return err
		}
		if runningJobs > 0 {
			return errPauseStillSettling
		}
		var failedJobs int64
		if err := tx.Model(&model.ProcessingJob{}).
			Where("video_id = ? AND status = ?", video.ID, model.JobStatusFailed).
			Count(&failedJobs).Error; err != nil {
			return err
		}
		if failedJobs > 0 {
			return errPausedVideoHasFailures
		}
		if err := tx.Where("video_id = ? AND status = ?", video.ID, model.JobStatusPending).
			Order("created_at").Find(&pendingJobs).Error; err != nil {
			return err
		}
		if len(pendingJobs) == 0 {
			return errNoPendingJobs
		}
		if video.PausedFromStatus != nil {
			restoredStatus = *video.PausedFromStatus
		}
		if restoredStatus != model.VideoStatusUploading && restoredStatus != model.VideoStatusProcessing {
			// 兼容迁移前的极端数据；URL prepare 尚未完成时应回到 uploading。
			restoredStatus = model.VideoStatusProcessing
			if video.UploadMethod == queue.UploadMethodURL {
				var prepareSucceeded int64
				if err := tx.Model(&model.ProcessingJob{}).
					Where("video_id = ? AND task_type = ? AND status = ?", video.ID, model.TaskPrepare, model.JobStatusSucceeded).
					Count(&prepareSucceeded).Error; err != nil {
					return err
				}
				if prepareSucceeded == 0 {
					restoredStatus = model.VideoStatusUploading
				}
			}
		}
		if err := tx.Model(&video).Updates(map[string]any{
			"status":             restoredStatus,
			"paused_from_status": nil,
		}).Error; err != nil {
			return err
		}
		// 恢复只为当前 pending attempt 追加新事件；Outbox UUID 允许同一 attempt 再派发。
		for _, job := range pendingJobs {
			if err := s.outbox.EnqueueJob(tx, job, video.UploadMethod, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		case errors.Is(err, errPauseStillSettling):
			c.JSON(http.StatusConflict, gin.H{"error": "video is still pausing; wait for running jobs to stop"})
		case errors.Is(err, errPausedVideoHasFailures):
			c.JSON(http.StatusConflict, gin.H{"error": "video has failed jobs and cannot be resumed"})
		case errors.Is(err, errNoPendingJobs):
			c.JSON(http.StatusConflict, gin.H{"error": "video has no pending jobs to resume"})
		case errors.Is(err, errVideoNotPaused):
			c.JSON(http.StatusConflict, gin.H{"error": "video is not paused"})
		default:
			fail(c, err)
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"video_id":    video.ID,
		"status":      restoredStatus,
		"queued_jobs": len(pendingJobs),
	})
}

// videoStatus 同时返回视频状态和最近一次处理任务状态，便于前端轮询。
func (s *Server) videoStatus(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if !ok {
		return
	}
	pauseSettled := true
	if v.Status == model.VideoStatusPaused {
		var runningJobs int64
		if err := s.db.Model(&model.ProcessingJob{}).
			Where("video_id = ? AND status = ?", v.ID, model.JobStatusRunning).
			Count(&runningJobs).Error; err != nil {
			fail(c, err)
			return
		}
		pauseSettled = runningJobs == 0
	}
	if c.Query("details") != "true" {
		c.JSON(http.StatusOK, gin.H{"video_id": v.ID, "status": v.Status, "error_message": v.ErrorMessage, "pause_settled": pauseSettled})
		return
	}
	var jobs []model.ProcessingJob
	result := s.db.Where("video_id = ?", v.ID).Order("task_type").Find(&jobs)
	if result.Error != nil {
		fail(c, result.Error)
		return
	}
	var indexSegmentCount int64
	// 索引片段必须按租户与视频双重限定，避免跨租户统计并保证返回真实落库数量。
	if err := s.db.Model(&model.VideoSegment{}).
		Where("tenant_id = ? AND video_id = ?", auth.UserID(c), v.ID).
		Count(&indexSegmentCount).Error; err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"video_id": v.ID, "status": v.Status, "error_message": v.ErrorMessage,
		"pause_settled": pauseSettled, "jobs": jobs,
		"processing_duration_ms": processingDurationMS(v.Status, jobs, time.Now().UTC()),
		"index_segment_count":    indexSegmentCount,
	})
}

// processingDurationMS 返回整条后台流水线的墙钟耗时；运行中持续增长，终态或暂停时冻结。
func processingDurationMS(videoStatus string, jobs []model.ProcessingJob, now time.Time) int64 {
	if len(jobs) == 0 {
		return 0
	}
	startedAt := jobs[0].CreatedAt
	finishedAt := jobs[0].UpdatedAt
	for _, job := range jobs[1:] {
		if job.CreatedAt.Before(startedAt) {
			startedAt = job.CreatedAt
		}
		if job.UpdatedAt.After(finishedAt) {
			finishedAt = job.UpdatedAt
		}
	}
	if videoStatus == model.VideoStatusUploading || videoStatus == model.VideoStatusProcessing {
		finishedAt = now
	}
	if finishedAt.Before(startedAt) {
		return 0
	}
	return finishedAt.Sub(startedAt).Milliseconds()
}

// summary 在独立的 Summary 任务成功后即可返回内容，不要求并行的 Embedding 已经完成。
// 视频总体 ready 仍由完整任务集合统一协调，摘要接口只开放已经落库并确认成功的阶段结果。
func (s *Server) summary(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if !ok {
		return
	}
	var summaryJob model.ProcessingJob
	if e := s.db.Select("status").Where(
		"tenant_id = ? AND video_id = ? AND task_type = ?",
		auth.UserID(c), v.ID, model.TaskSummary,
	).First(&summaryJob).Error; e != nil {
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			fail(c, e)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "summary not ready"})
		return
	}
	if summaryJob.Status != model.JobStatusSucceeded {
		// 不能仅凭 video_summaries 中存在记录开放读取，避免重跑期间返回旧摘要。
		c.JSON(http.StatusNotFound, gin.H{"error": "summary not ready"})
		return
	}
	var x model.VideoSummary
	if e := s.db.Where("video_id = ?", v.ID).First(&x).Error; e != nil {
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			fail(c, e)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "summary not ready"})
		return
	}
	c.JSON(http.StatusOK, x)
}

// createConversation 通过 ownedVideo 确保对话只能建立在用户自己的视频上。
func (s *Server) createConversation(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if !ok {
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	_ = c.ShouldBindJSON(&in)
	x := model.Conversation{TenantID: auth.UserID(c), VideoID: v.ID, Title: in.Title}
	if e := s.db.Create(&x).Error; e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusCreated, x)
}

// ownedConversation 在数据库层进行租户过滤，形成会话接口的授权边界。
func (s *Server) ownedConversation(c *gin.Context) (model.Conversation, bool) {
	var x model.Conversation
	if e := s.db.Where("id = ? AND tenant_id = ?", c.Param("id"), auth.UserID(c)).First(&x).Error; e != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return x, false
	}
	return x, true
}

// createMessage 使用 SSE 推送回答增量，并在 done 事件中返回已持久化的完整消息。
func (s *Server) createMessage(c *gin.Context) {
	x, ok := s.ownedConversation(c)
	if !ok {
		return
	}
	var in struct {
		ClientMessageID string                    `json:"client_message_id" binding:"required"`
		Content         string                    `json:"content" binding:"required"`
		Vision          chatservice.VisionRequest `json:"vision"`
	}
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "client_message_id and content are required")
		return
	}
	if _, err := uuid.Parse(in.ClientMessageID); err != nil {
		bad(c, "client_message_id must be a UUID")
		return
	}
	content, err := s.chat.ValidateContent(in.Content)
	if err != nil {
		bad(c, err.Error())
		return
	}
	if err := s.chat.ValidateVisionRequest(c.Request.Context(), auth.UserID(c), x.VideoID, in.Vision); err != nil {
		writeVisionRequestError(c, err)
		return
	}
	streamStarted := false
	startStream := func() {
		if streamStarted {
			return
		}
		streamStarted = true
		c.Header("Content-Type", "text/event-stream; charset=utf-8")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}
	writeEvent := func(event string, payload any) error {
		startStream()
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		// 每个事件写完立即 Flush，让浏览器无需等待整个模型回答完成。
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}

	result, err := s.chat.ReplyStream(c.Request.Context(), chatservice.ReplyInput{
		TenantID: auth.UserID(c), Conversation: x, ClientMessageID: in.ClientMessageID, Content: content, Vision: in.Vision,
		OnProgress: func(progress visionagent.Progress) error {
			return writeEvent("progress", progress)
		},
	}, func(delta string) error {
		return writeEvent("delta", gin.H{"content": delta})
	})
	if err != nil {
		if streamStarted {
			// 流式错误只记录安全类别，避免本地路径、对象键或上游响应进入日志。
			log.Printf("stream chat reply failed: %T", err)
			_ = writeEvent("error", gin.H{"error": "回答生成失败，请重试"})
			return
		}
		if errors.Is(err, chatservice.ErrConversationBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": "conversation is generating another reply"})
			return
		}
		if errors.Is(err, chatservice.ErrIdempotencyPayloadMismatch) {
			c.JSON(http.StatusConflict, gin.H{"error": "client_message_id payload does not match the original request", "code": "IDEMPOTENCY_PAYLOAD_MISMATCH"})
			return
		}
		if errors.Is(err, chatservice.ErrVisionNotReady) {
			c.JSON(http.StatusConflict, gin.H{"error": "画面理解所需的视频内容尚未准备好", "code": "VISION_NOT_READY"})
			return
		}
		if errors.Is(err, chatservice.ErrVisionUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "画面理解服务暂时不可用", "code": "VISION_UNAVAILABLE"})
			return
		}
		if errors.Is(err, visionagent.ErrConcurrentLimit) || errors.Is(err, visionagent.ErrRateLimit) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "画面理解请求过于频繁，请稍后重试", "code": "VISION_RATE_LIMITED"})
			return
		}
		fail(c, err)
		return
	}
	if err := writeEvent("done", result); err != nil {
		log.Printf("write completed chat stream failed: %v", err)
	}
}

func writeVisionRequestError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, chatservice.ErrVisionRangeRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "开启画面理解后必须填写视频时间区间", "code": "VISION_RANGE_REQUIRED"})
	case errors.Is(err, chatservice.ErrVisionRangeInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "画面理解时间区间无效", "code": "VISION_RANGE_INVALID"})
	case errors.Is(err, chatservice.ErrVisionRangeTooLong):
		c.JSON(http.StatusBadRequest, gin.H{"error": "画面理解时间区间不能超过30秒", "code": "VISION_RANGE_TOO_LONG"})
	case errors.Is(err, chatservice.ErrVisionRangeOutOfBounds):
		c.JSON(http.StatusBadRequest, gin.H{"error": "画面理解时间区间超过视频时长", "code": "VISION_RANGE_OUT_OF_BOUNDS"})
	case errors.Is(err, chatservice.ErrVisionUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "画面理解服务暂时不可用", "code": "VISION_UNAVAILABLE"})
	default:
		fail(c, err)
	}
}

// messages 按创建时间返回已授权会话中的完整消息历史。
func (s *Server) messages(c *gin.Context) {
	x, ok := s.ownedConversation(c)
	if !ok {
		return
	}
	var all []model.Message
	if e := s.db.Where("conversation_id = ? AND (role <> ? OR generation_status = ?)", x.ID, "user", "completed").
		Order("created_at ASC, id ASC").Find(&all).Error; e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": all})
}

// bad 统一返回安全的客户端参数错误。
func bad(c *gin.Context, msg string) { c.JSON(http.StatusBadRequest, gin.H{"error": msg}) }

// allowUpload 统一限制本地文件和 URL 视频创建；Redis 故障时失败关闭，不能绕过额度。
func (s *Server) allowUpload(c *gin.Context) bool {
	if s.uploadLimiter == nil {
		log.Printf("upload rate limiter is not configured")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传限流服务暂时不可用", "code": "UPLOAD_RATE_LIMIT_UNAVAILABLE"})
		return false
	}
	result, err := s.uploadLimiter.Allow(c.Request.Context(), auth.UserID(c))
	if err != nil {
		log.Printf("consume upload rate limit token: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传限流服务暂时不可用", "code": "UPLOAD_RATE_LIMIT_UNAVAILABLE"})
		return false
	}
	c.Header("X-RateLimit-Limit", strconv.Itoa(s.uploadLimitCapacity))
	c.Header("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
	if result.Allowed {
		return true
	}
	retryAfterSeconds := int((result.RetryAfter + time.Second - 1) / time.Second)
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": "上传过于频繁，请稍后重试", "code": "UPLOAD_RATE_LIMITED", "retry_after_seconds": retryAfterSeconds,
	})
	return false
}

// fail 不向客户端泄露底层数据库或基础设施错误详情。
func fail(c *gin.Context, e error) {
	log.Printf("api request failed: %v", e)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}
