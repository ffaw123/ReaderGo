package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"readergo/internal/auth"
	"readergo/internal/model"
	"readergo/internal/queue"
	"readergo/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Server 汇集 HTTP 接口依赖，避免处理器直接依赖全局变量。
type Server struct {
	db       *gorm.DB
	auth     *auth.Service
	jobs     *queue.Producer
	uploads  *storage.MultipartStore
	partSize int64
}

const (
	// 首批 URL 与建议的最大并发数一致，避免在初始化时暴露全部分片授权。
	initialUploadURLCount = 8
	// 补充 URL 时允许略多于单轮并发数，以减少网络往返且限制凭证数量。
	maxPartURLBatch = 16
	// S3 Multipart Upload 协议规定的最大分片数。
	maxMultipartParts = 10000
)

// createUploadRequest 仅接收客户端元数据，文件二进制不会经过 Gin 服务。
type createUploadRequest struct {
	Filename    string `json:"filename" binding:"required,max=512"`
	ContentType string `json:"content_type"`
	FileSize    int64  `json:"file_size" binding:"required,gt=0"`
	Title       string `json:"title"`
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
		bad(c, "filename and a positive file_size are required")
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
	// 预先生成视频 ID，才能构造包含租户和视频归属的对象路径。
	videoID := uuid.NewString()
	// 使用固定对象名，不信任 filename 的扩展名；上传完成后由 FFprobe 校验真实媒体格式。
	objectKey := fmt.Sprintf("tenants/%s/videos/%s/source/original", tenantID, videoID)
	// 先创建 uploading 视频记录，让后续完成上传流程拥有稳定的资源归属。
	v := model.Video{ID: videoID, TenantID: tenantID, SourceURL: "local://" + in.Filename, Title: in.Title, Status: "uploading", ObjectKey: objectKey}
	if e := s.db.Create(&v).Error; e != nil {
		fail(c, e)
		return
	}
	// uploadID 是完成 Multipart Upload 所必需的秘密服务端状态，绝不返回给客户端。
	uploadID, e := s.uploads.Create(c.Request.Context(), objectKey, in.ContentType)
	if e != nil {
		// MinIO 初始化失败时移除刚插入的视频，避免留下不可恢复的 uploading 记录。
		_ = s.db.Delete(&v).Error
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not initialize object upload"})
		return
	}
	// 上传会话独立保存分片布局、MinIO uploadID 和过期时间，不能只依赖 videos.status。
	session := model.UploadSession{TenantID: tenantID, VideoID: videoID, UploadID: uploadID, ObjectKey: objectKey, PartSize: s.partSize, FileSize: in.FileSize, PartCount: partCount, Status: "uploading", ExpiresAt: time.Now().Add(24 * time.Hour)}
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
		// URL 无法交付时该会话无法被客户端使用，数据库和对象存储均需清理。
		_ = s.uploads.Abort(c.Request.Context(), objectKey, uploadID)
		_ = s.db.Delete(&session).Error
		_ = s.db.Delete(&v).Error
		fail(c, e)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"upload_id": session.ID, "video_id": videoID, "part_size": s.partSize, "part_count": partCount, "expires_at": session.ExpiresAt, "parts": parts})
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
	if session.Status != "uploading" || time.Now().After(session.ExpiresAt) {
		c.JSON(http.StatusConflict, gin.H{"error": "upload session is not active"})
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
		fail(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"upload_id": session.ID, "parts": parts})
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

// New 使用数据库、鉴权服务和消息生产者创建 API 服务。
func New(db *gorm.DB, authService *auth.Service, jobs *queue.Producer, uploads *storage.MultipartStore, partSize int64) *Server {
	return &Server{db: db, auth: authService, jobs: jobs, uploads: uploads, partSize: partSize}
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
	p.POST("/uploads/:id/parts", s.uploadPartURLs)
	p.GET("/videos/:id", s.getVideo) // 这里的id是指视频id
	p.GET("/videos/:id/status", s.videoStatus)
	p.GET("/videos/:id/summary", s.summary)
	p.POST("/videos/:id/conversations", s.createConversation)
	p.POST("/conversations/:id/messages", s.createMessage) // 这里的id是会话id
	p.GET("/conversations/:id/messages", s.messages)
	return r
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

// createVideo 先持久化视频与任务，再发送 Kafka 消息，避免 Worker 找不到任务记录。
func (s *Server) createVideo(c *gin.Context) {
	var in createVideoRequest
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "source_url must be a valid URL")
		return
	}
	// 仅接收 HTTP(S) URL，后续下载器也应防范内网地址（SSRF）。
	u, e := url.Parse(in.SourceURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") {
		bad(c, "only http(s) video URLs are allowed")
		return
	}
	tenant := auth.UserID(c)
	v := model.Video{TenantID: tenant, SourceURL: in.SourceURL, Title: in.Title, Status: "created"}
	if e = s.db.Create(&v).Error; e != nil {
		fail(c, e)
		return
	}
	j := model.ProcessingJob{TenantID: tenant, VideoID: v.ID, TaskType: "process_video", Status: "pending", Attempt: 1}
	if e = s.db.Create(&j).Error; e != nil {
		fail(c, e)
		return
	}
	if e = s.jobs.Publish(context.Background(), queue.VideoJobMessage{JobID: j.ID, TenantID: tenant, VideoID: v.ID, TaskType: j.TaskType, Attempt: j.Attempt}); e != nil {
		// 生产消息失败时将持久化状态标为失败，避免任务长期卡在 pending。
		_ = s.db.Model(&j).Updates(map[string]any{"status": "failed", "error_message": e.Error()}).Error
		_ = s.db.Model(&v).Update("status", "failed").Error
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not queue processing job"})
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

// videoStatus 同时返回视频状态和最近一次处理任务状态，便于前端轮询。
func (s *Server) videoStatus(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if !ok {
		return
	}
	var j model.ProcessingJob
	s.db.Where("video_id = ?", v.ID).Order("created_at DESC").First(&j)
	c.JSON(http.StatusOK, gin.H{"video_id": v.ID, "status": v.Status, "error_message": v.ErrorMessage, "job": j})
}

// summary 只有处理完成并写入摘要后才返回内容。
func (s *Server) summary(c *gin.Context) {
	v, ok := s.ownedVideo(c)
	if !ok {
		return
	}
	var x model.VideoSummary
	if e := s.db.Where("video_id = ?", v.ID).First(&x).Error; e != nil {
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

// createMessage 当前先保存用户问题；后续 RAG Worker 将追加 assistant 消息与引用。
func (s *Server) createMessage(c *gin.Context) {
	x, ok := s.ownedConversation(c)
	if !ok {
		return
	}
	var in struct {
		Content string `json:"content" binding:"required,max=10000"`
	}
	if c.ShouldBindJSON(&in) != nil {
		bad(c, "content is required")
		return
	}
	m := model.Message{ConversationID: x.ID, Role: "user", Content: in.Content, Citations: []byte("[]")}
	if e := s.db.Create(&m).Error; e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusAccepted, m)
}

// messages 按创建时间返回已授权会话中的完整消息历史。
func (s *Server) messages(c *gin.Context) {
	x, ok := s.ownedConversation(c)
	if !ok {
		return
	}
	var all []model.Message
	if e := s.db.Where("conversation_id = ?", x.ID).Order("created_at").Find(&all).Error; e != nil {
		fail(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": all})
}

// bad 统一返回安全的客户端参数错误。
func bad(c *gin.Context, msg string) { c.JSON(http.StatusBadRequest, gin.H{"error": msg}) }

// fail 不向客户端泄露底层数据库或基础设施错误详情。
func fail(c *gin.Context, e error) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}
