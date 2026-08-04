package model

import "time"

const (
	VideoStatusUploading  = "uploading"
	VideoStatusProcessing = "processing"
	VideoStatusPaused     = "paused"
	VideoStatusReady      = "ready"
	VideoStatusFailed     = "failed"

	JobStatusPending   = "pending"
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"

	TaskPrepare  = "prepare"
	TaskASR      = "asr"
	TaskVision   = "vision"
	TaskRAGIndex = "rag_index"
	TaskSummary  = "summary"
)

// User 同时表示登录身份和第一版中的租户隔离边界。
type User struct {
	ID           string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	Email        string    `gorm:"uniqueIndex;not null" json:"email"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"display_name"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Video 记录提交的视频来源及其异步处理生命周期。
// TenantID 永不序列化，调用方不能获知或伪造资源归属。
type Video struct {
	ID        string `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID  string `gorm:"type:uuid;not null;index" json:"-"`
	SourceURL string `json:"source_url"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	// UploadMethod 持久化源文件进入方式，供暂停恢复时安全重建 RabbitMQ 消息。
	UploadMethod     string    `json:"-"`
	PausedFromStatus *string   `json:"paused_from_status,omitempty"`
	DurationMS       int64     `json:"duration_ms"`
	ObjectKey        string    `json:"object_key"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ProcessingJob 记录由 RabbitMQ 通知执行的处理任务；PostgreSQL 始终是任务状态事实来源。
type ProcessingJob struct {
	ID             string     `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID       string     `gorm:"type:uuid;not null" json:"-"`
	VideoID        string     `gorm:"type:uuid;not null" json:"video_id"`
	TaskType       string     `json:"task_type"`
	Attempt        int        `json:"attempt"`
	Status         string     `json:"status"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	LeaseOwner     string     `json:"-"`
	LeaseExpiresAt *time.Time `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const (
	OutboxStatusPending    = "pending"
	OutboxStatusPublishing = "publishing"
	OutboxStatusPublished  = "published"
)

// OutboxMessage 将数据库状态变更与待发布通知放在同一事务中，避免提交后进程崩溃造成漏消息。
type OutboxMessage struct {
	ID              string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	TenantID        string    `gorm:"type:uuid;not null;index"`
	VideoID         string    `gorm:"type:uuid;not null;index"`
	JobID           string    `gorm:"type:uuid;not null;index"`
	TaskType        string    `gorm:"not null"`
	Attempt         int       `gorm:"not null"`
	ExchangeName    string    `gorm:"not null"`
	RoutingKey      string    `gorm:"not null"`
	Payload         string    `gorm:"type:jsonb;not null"`
	Status          string    `gorm:"not null"`
	AvailableAt     time.Time `gorm:"not null"`
	PublishAttempts int       `gorm:"not null"`
	LastError       string    `gorm:"not null"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
	PublishedAt     *time.Time
}

func (OutboxMessage) TableName() string { return "message_outbox" }

// ASRSegment 保存 ASR 阶段的原始转写结果，不等同于最终的 RAG 检索片段。
// start_ms 与 end_ms 均相对视频起点，使用 [start_ms, end_ms) 左闭右开区间。
type ASRSegment struct {
	ID         string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID   string    `gorm:"type:uuid;not null;index" json:"-"`
	VideoID    string    `gorm:"type:uuid;not null;index" json:"video_id"`
	StartMS    int64     `gorm:"not null" json:"start_ms"`
	EndMS      int64     `gorm:"not null" json:"end_ms"`
	Transcript string    `gorm:"not null" json:"transcript"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName 显式指定原始转写表，避免和 RAG 最终使用的 video_segments 混淆。
func (ASRSegment) TableName() string { return "asr_segments" }

const (
	OCRStatusSucceeded          = "succeeded"
	OCRStatusNoText             = "no_text"
	OCRStatusServiceUnavailable = "service_unavailable"
)

// OCRSegment 保存抽样画面上的原始 OCR 结果。
// Text 只包含真实识别文字；无可用文字或服务不可用时保持空字符串，由 Status 说明原因。
type OCRSegment struct {
	ID         string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID   string    `gorm:"type:uuid;not null;index" json:"-"`
	VideoID    string    `gorm:"type:uuid;not null;index" json:"video_id"`
	StartMS    int64     `gorm:"not null" json:"start_ms"`
	EndMS      int64     `gorm:"not null" json:"end_ms"`
	Text       string    `gorm:"not null" json:"text"`
	Confidence float64   `gorm:"not null" json:"confidence"`
	Status     string    `gorm:"not null" json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

func (OCRSegment) TableName() string { return "ocr_segments" }

// VideoSegment 是 rag_index 生成的固定时间窗口；原始证据与检索代理分开保存。
type VideoSegment struct {
	ID             string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID       string    `gorm:"type:uuid;not null;index" json:"-"`
	VideoID        string    `gorm:"type:uuid;not null;index" json:"video_id"`
	StartMS        int64     `gorm:"not null" json:"start_ms"`
	EndMS          int64     `gorm:"not null" json:"end_ms"`
	Transcript     string    `gorm:"not null" json:"transcript"`
	OCRText        string    `gorm:"not null" json:"ocr_text"`
	VisualCaption  string    `gorm:"not null" json:"visual_caption"`
	SegmentSummary string    `gorm:"not null" json:"segment_summary"`
	KeyFacts       []string  `gorm:"serializer:json;type:jsonb;not null" json:"key_facts"`
	Keywords       []string  `gorm:"serializer:json;type:jsonb;not null" json:"keywords"`
	RetrievalText  string    `gorm:"not null" json:"retrieval_text"`
	SourceHash     string    `gorm:"not null" json:"source_hash"`
	PromptVersion  string    `gorm:"not null" json:"prompt_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SegmentEmbedding 为每个检索片段保存一份主语义向量。
// Processor 使用显式 vector 类型写入，避免把向量误存为 PostgreSQL 数组或 JSON。
type SegmentEmbedding struct {
	ID             string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID       string    `gorm:"type:uuid;not null;index" json:"-"`
	VideoID        string    `gorm:"type:uuid;not null;index" json:"video_id"`
	SegmentID      string    `gorm:"type:uuid;not null;uniqueIndex" json:"segment_id"`
	EmbeddingModel string    `gorm:"not null" json:"embedding_model"`
	Embedding      string    `gorm:"type:vector(1024);not null" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

type UploadSession struct {
	ID        string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID  string    `gorm:"type:uuid;not null;index" json:"-"`
	VideoID   string    `gorm:"type:uuid;not null;index" json:"video_id"`
	UploadID  string    `gorm:"not null" json:"-"`
	ObjectKey string    `gorm:"not null" json:"object_key"`
	PartSize  int64     `gorm:"not null" json:"part_size"`
	FileSize  int64     `gorm:"not null" json:"file_size"`
	PartCount int       `gorm:"not null" json:"part_count"`
	Status    string    `gorm:"not null" json:"status"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VideoSummary 保存一个视频唯一的最终 LLM 摘要。
type VideoSummary struct {
	ID        string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	VideoID   string    `gorm:"type:uuid;uniqueIndex" json:"video_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Conversation 将一段多轮问答限定在单个视频范围内。
type Conversation struct {
	ID        string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID  string    `gorm:"type:uuid;not null" json:"-"`
	VideoID   string    `gorm:"type:uuid;not null" json:"video_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message 保存用户问题与未来的助手回答，并以 JSON 保存时间戳引用。
type Message struct {
	ID             string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	ConversationID string    `gorm:"type:uuid;not null" json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	Citations      []byte    `gorm:"type:jsonb" json:"citations"`
	CreatedAt      time.Time `json:"created_at"`
}
