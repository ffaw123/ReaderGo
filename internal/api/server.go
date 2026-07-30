package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"readergo/internal/auth"
	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Server 汇集 HTTP 接口依赖，避免处理器直接依赖全局变量。
type Server struct {
	db   *gorm.DB
	auth *auth.Service
	jobs *queue.Producer
}

// New 使用数据库、鉴权服务和消息生产者创建 API 服务。
func New(db *gorm.DB, authService *auth.Service, jobs *queue.Producer) *Server {
	return &Server{db: db, auth: authService, jobs: jobs}
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
