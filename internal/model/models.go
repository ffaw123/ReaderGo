package model

import "time"

const (
	VideoStatusUploading  = "uploading"
	VideoStatusProcessing = "processing"
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
	ID           string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID     string    `gorm:"type:uuid;not null;index" json:"-"`
	SourceURL    string    `json:"source_url"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	DurationMS   int64     `json:"duration_ms"`
	ObjectKey    string    `json:"object_key"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProcessingJob 记录由 Kafka 分发的处理任务及其重试次数。
type ProcessingJob struct {
	ID           string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	TenantID     string    `gorm:"type:uuid;not null" json:"-"`
	VideoID      string    `gorm:"type:uuid;not null" json:"video_id"`
	TaskType     string    `json:"task_type"`
	Attempt      int       `json:"attempt"`
	Status       string    `json:"status"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

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
