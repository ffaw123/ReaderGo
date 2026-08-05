package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config 保存 API 与 Worker 共用的运行时配置。
// 密钥只从环境变量读取，避免写入源代码。
type Config struct {
	HTTPAddr, DatabaseURL, JWTSecret              string
	JWTTTLHours                                   int
	MinIOEndpoint, MinIOAccessKey, MinIOSecretKey string
	MinIOBucket                                   string
	MinIOSecure                                   bool
	RabbitMQURL                                   string
	RabbitMQJobsExchange                          string
	RabbitMQDLXExchange                           string
	RabbitMQPrefetch                              int
	RabbitMQConsumerTimeout                       time.Duration
	JobMaxAttempts                                int
	JobRetryDelays                                []time.Duration
	JobLeaseTTL                                   time.Duration
	JobHeartbeatInterval                          time.Duration
	OutboxPollInterval                            time.Duration
	OutboxBatchSize                               int
	// FFmpegPath 是 FFmpeg 可执行文件路径；Worker 启动时会校验它及同目录的 ffprobe。
	FFmpegPath string
	// ASR 配置保持厂商无关，通过 OpenAI 兼容客户端调用 Chat Completions 接口。
	ASRAPIKey, ASRBaseURL, ASRModel string
	ASRConcurrency                  int
	// OCR 使用百炼 OpenAI 兼容接口调用 Qwen-VL-OCR，不复用 ASR 或 RAG 的凭据和模型。
	OCRAPIKey, OCRBaseURL, OCRModel string
	OCRSceneThreshold               float64
	OCRHashDistance                 int
	OCRConcurrency                  int
	// RAG 使用 OpenAI 兼容的文本生成与 Embedding 接口，和 ASR 服务保持独立配置。
	RAGAPIKey, RAGBaseURL, RAGChatModel, RAGEmbeddingModel string
	RAGConcurrency                                         int
	RAGRequestTimeout                                      time.Duration
	// Chat 回答模型与后台 RAG 处理完全隔离，便于面向用户单独控制成本和供应商。
	ChatAPIKey, ChatBaseURL, ChatModel string
	ChatRequestTimeout                 time.Duration
	// Intent 模型不通过用户界面暴露配置，后续可以无缝替换成本地 OpenAI 兼容服务。
	IntentAPIKey, IntentBaseURL, IntentModel string
	IntentRequestTimeout                     time.Duration
	IntentConfidenceThreshold                float64
	ChatRAGMinSimilarity                     float64
	ChatMaxInputRunes, ChatHistoryTurns      int
	// UploadPartSizeMiB 限制为 16-64 MiB，以平衡重试成本和请求数量。
	UploadPartSizeMiB, UploadURLTTLMinutes int
}

// Load 优先加载本地 .env，再校验生产环境必填配置。
func Load() (Config, error) {
	_ = godotenv.Load()
	retryDelays, err := durationListValue("JOB_RETRY_DELAYS", "30s,5m,30m")
	if err != nil {
		return Config{}, err
	}
	rabbitPrefetch, err := strictIntValue("RABBITMQ_PREFETCH", 1)
	if err != nil {
		return Config{}, err
	}
	jobMaxAttempts, err := strictIntValue("JOB_MAX_ATTEMPTS", 4)
	if err != nil {
		return Config{}, err
	}
	outboxBatchSize, err := strictIntValue("OUTBOX_BATCH_SIZE", 100)
	if err != nil {
		return Config{}, err
	}
	intentConfidenceThreshold, err := strictFloatValue("INTENT_CONFIDENCE_THRESHOLD", 0.75)
	if err != nil {
		return Config{}, err
	}
	chatRAGMinSimilarity, err := strictFloatValue("CHAT_RAG_MIN_SIMILARITY", 0.25)
	if err != nil {
		return Config{}, err
	}
	chatMaxInputRunes, err := strictIntValue("CHAT_MAX_INPUT_RUNES", 4000)
	if err != nil {
		return Config{}, err
	}
	chatHistoryTurns, err := strictIntValue("CHAT_HISTORY_TURNS", 3)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		HTTPAddr:    value("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		JWTTTLHours: intValue("JWT_TTL_HOURS", 24),

		MinIOEndpoint:  os.Getenv("MINIO_ENDPOINT"),
		MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"),
		MinIOBucket:    value("MINIO_BUCKET", "videos"),
		MinIOSecure:    boolValue("MINIO_SECURE", false),

		RabbitMQURL:             value("RABBITMQ_URL", "amqp://readergo:readergo-secret@localhost:5672/readergo"),
		RabbitMQJobsExchange:    value("RABBITMQ_JOBS_EXCHANGE", "readergo.video.jobs"),
		RabbitMQDLXExchange:     value("RABBITMQ_DLX_EXCHANGE", "readergo.video.dlx"),
		RabbitMQPrefetch:        rabbitPrefetch,
		RabbitMQConsumerTimeout: durationMillisecondsValue("RABBITMQ_CONSUMER_TIMEOUT_MS", 6*time.Hour),

		JobMaxAttempts:       jobMaxAttempts,
		JobRetryDelays:       retryDelays,
		JobLeaseTTL:          durationValue("JOB_LEASE_TTL", 2*time.Minute),
		JobHeartbeatInterval: durationValue("JOB_HEARTBEAT_INTERVAL", 30*time.Second),
		OutboxPollInterval:   durationMillisecondsValue("OUTBOX_POLL_INTERVAL_MS", 500*time.Millisecond),
		OutboxBatchSize:      outboxBatchSize,

		FFmpegPath:     value("FFMPEG_PATH", "ffmpeg"),
		ASRAPIKey:      os.Getenv("ASR_API_KEY"),
		ASRBaseURL:     value("ASR_BASE_URL", "https://api.xiaomimimo.com/v1"),
		ASRModel:       value("ASR_MODEL", "mimo-v2.5-asr"),
		ASRConcurrency: intValue("ASR_CONCURRENCY", 10),

		OCRAPIKey:         os.Getenv("OCR_API_KEY"),
		OCRBaseURL:        os.Getenv("OCR_BASE_URL"),
		OCRModel:          value("OCR_MODEL", "qwen-vl-ocr"),
		OCRSceneThreshold: floatValue("OCR_SCENE_THRESHOLD", 0.3),
		OCRHashDistance:   intValue("OCR_HASH_DISTANCE", 5),
		OCRConcurrency:    intValue("OCR_CONCURRENCY", 10),

		RAGAPIKey:         os.Getenv("RAG_API_KEY"),
		RAGBaseURL:        value("RAG_BASE_URL", "https://api.openai.com/v1"),
		RAGChatModel:      value("RAG_CHAT_MODEL", "gpt-4.1-mini"),
		RAGEmbeddingModel: value("RAG_EMBEDDING_MODEL", "text-embedding-3-small"),
		RAGConcurrency:    intValue("RAG_CONCURRENCY", 10),
		RAGRequestTimeout: durationValue("RAG_REQUEST_TIMEOUT", 2*time.Minute),

		ChatAPIKey:           os.Getenv("CHAT_API_KEY"),
		ChatBaseURL:          value("CHAT_BASE_URL", "https://api.openai.com/v1"),
		ChatModel:            value("CHAT_MODEL", "gpt-4.1-mini"),
		ChatRequestTimeout:   durationValue("CHAT_REQUEST_TIMEOUT", time.Minute),
		ChatRAGMinSimilarity: chatRAGMinSimilarity,
		ChatMaxInputRunes:    chatMaxInputRunes,
		ChatHistoryTurns:     chatHistoryTurns,

		IntentAPIKey:              os.Getenv("INTENT_API_KEY"),
		IntentBaseURL:             value("INTENT_BASE_URL", "https://api.openai.com/v1"),
		IntentModel:               value("INTENT_MODEL", "gpt-4.1-mini"),
		IntentRequestTimeout:      durationValue("INTENT_REQUEST_TIMEOUT", 20*time.Second),
		IntentConfidenceThreshold: intentConfidenceThreshold,

		UploadPartSizeMiB:   intValue("UPLOAD_PART_SIZE_MIB", 32),
		UploadURLTTLMinutes: intValue("UPLOAD_URL_TTL_MINUTES", 15),
	}
	if c.DatabaseURL == "" || len(c.JWTSecret) < 32 || c.MinIOEndpoint == "" || c.MinIOAccessKey == "" || c.MinIOSecretKey == "" {
		return Config{}, fmt.Errorf("DATABASE_URL, MinIO credentials, and a JWT_SECRET of at least 32 characters are required")
	}
	if c.UploadPartSizeMiB < 16 || c.UploadPartSizeMiB > 64 {
		return Config{}, fmt.Errorf("UPLOAD_PART_SIZE_MIB must be between 16 and 64")
	}
	parsedRabbitURL, err := url.Parse(c.RabbitMQURL)
	if err != nil || (parsedRabbitURL.Scheme != "amqp" && parsedRabbitURL.Scheme != "amqps") || parsedRabbitURL.Host == "" {
		return Config{}, fmt.Errorf("RABBITMQ_URL must be a valid amqp or amqps URL")
	}
	if strings.TrimSpace(c.RabbitMQJobsExchange) == "" || strings.TrimSpace(c.RabbitMQDLXExchange) == "" {
		return Config{}, fmt.Errorf("RabbitMQ exchange names must not be empty")
	}
	if c.RabbitMQPrefetch < 1 || c.RabbitMQPrefetch > 1000 {
		return Config{}, fmt.Errorf("RABBITMQ_PREFETCH must be between 1 and 1000")
	}
	if c.RabbitMQConsumerTimeout <= 0 {
		return Config{}, fmt.Errorf("RABBITMQ_CONSUMER_TIMEOUT_MS must be positive")
	}
	if c.JobMaxAttempts < 1 || c.JobMaxAttempts > 100 || len(c.JobRetryDelays) < c.JobMaxAttempts-1 {
		return Config{}, fmt.Errorf("JOB_RETRY_DELAYS must provide at least JOB_MAX_ATTEMPTS - 1 positive delays")
	}
	// 心跳必须显著早于租约到期，避免正常长任务被 Reconciler 误回收。
	if c.JobLeaseTTL <= 0 || c.JobHeartbeatInterval <= 0 || c.JobHeartbeatInterval*2 >= c.JobLeaseTTL {
		return Config{}, fmt.Errorf("JOB_HEARTBEAT_INTERVAL must be positive and less than half of JOB_LEASE_TTL")
	}
	if c.OutboxPollInterval <= 0 || c.OutboxBatchSize < 1 || c.OutboxBatchSize > 1000 {
		return Config{}, fmt.Errorf("Outbox poll interval and batch size must be within valid bounds")
	}
	if c.ASRConcurrency < 1 || c.ASRConcurrency > 10 {
		return Config{}, fmt.Errorf("ASR_CONCURRENCY must be between 1 and 10")
	}
	if c.OCRSceneThreshold <= 0 || c.OCRSceneThreshold > 1 {
		return Config{}, fmt.Errorf("OCR_SCENE_THRESHOLD must be greater than 0 and at most 1")
	}
	if c.OCRHashDistance < 0 || c.OCRHashDistance > 64 {
		return Config{}, fmt.Errorf("OCR_HASH_DISTANCE must be between 0 and 64")
	}
	if c.OCRConcurrency < 1 || c.OCRConcurrency > 10 {
		return Config{}, fmt.Errorf("OCR_CONCURRENCY must be between 1 and 10")
	}
	if c.RAGConcurrency < 1 || c.RAGConcurrency > 10 {
		return Config{}, fmt.Errorf("RAG_CONCURRENCY must be between 1 and 10")
	}
	if c.RAGRequestTimeout <= 0 {
		return Config{}, fmt.Errorf("RAG_REQUEST_TIMEOUT must be positive")
	}
	if c.ChatRequestTimeout <= 0 || c.IntentRequestTimeout <= 0 {
		return Config{}, fmt.Errorf("CHAT_REQUEST_TIMEOUT and INTENT_REQUEST_TIMEOUT must be positive")
	}
	if c.IntentConfidenceThreshold <= 0 || c.IntentConfidenceThreshold > 1 {
		return Config{}, fmt.Errorf("INTENT_CONFIDENCE_THRESHOLD must be greater than 0 and at most 1")
	}
	if c.ChatRAGMinSimilarity < -1 || c.ChatRAGMinSimilarity > 1 {
		return Config{}, fmt.Errorf("CHAT_RAG_MIN_SIMILARITY must be between -1 and 1")
	}
	if c.ChatMaxInputRunes < 1 || c.ChatMaxInputRunes > 20000 || c.ChatHistoryTurns < 1 || c.ChatHistoryTurns > 10 {
		return Config{}, fmt.Errorf("CHAT_MAX_INPUT_RUNES and CHAT_HISTORY_TURNS are outside the supported range")
	}
	// 预签名 URL 的过期时间远短于上传会话，客户端可在失效后重新申请同一分片的 URL。
	if c.UploadURLTTLMinutes < 1 || c.UploadURLTTLMinutes > 60 {
		return Config{}, fmt.Errorf("UPLOAD_URL_TTL_MINUTES must be between 1 and 60")
	}
	return c, nil
}

func boolValue(k string, fallback bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	b, e := strconv.ParseBool(v)
	return e == nil && b
}

// value 仅在环境变量未设置时返回默认值。
func value(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// intValue 在可选数字配置格式错误时回退默认值，避免服务无法启动。
func intValue(k string, fallback int) int {
	v, e := strconv.Atoi(value(k, strconv.Itoa(fallback)))
	if e != nil {
		return fallback
	}
	return v
}

func strictIntValue(k string, fallback int) (int, error) {
	v := value(k, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", k)
	}
	return parsed, nil
}

func floatValue(k string, fallback float64) float64 {
	v, e := strconv.ParseFloat(value(k, strconv.FormatFloat(fallback, 'f', -1, 64)), 64)
	if e != nil {
		return fallback
	}
	return v
}

func strictFloatValue(k string, fallback float64) (float64, error) {
	parsed, err := strconv.ParseFloat(value(k, strconv.FormatFloat(fallback, 'f', -1, 64)), 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", k)
	}
	return parsed, nil
}

func durationValue(k string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(value(k, fallback.String()))
	if err != nil {
		return 0
	}
	return d
}

func durationMillisecondsValue(k string, fallback time.Duration) time.Duration {
	milliseconds, err := strconv.ParseInt(value(k, strconv.FormatInt(fallback.Milliseconds(), 10)), 10, 64)
	if err != nil {
		return 0
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func durationListValue(k, fallback string) ([]time.Duration, error) {
	parts := strings.Split(value(k, fallback), ",")
	delays := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		delay, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil || delay <= 0 {
			return nil, fmt.Errorf("%s must contain comma-separated positive durations", k)
		}
		delays = append(delays, delay)
	}
	return delays, nil
}
