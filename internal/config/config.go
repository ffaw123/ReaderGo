package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

// Config 保存 API 与 Worker 共用的运行时配置。
// 密钥只从环境变量读取，避免写入源代码。
type Config struct {
	HTTPAddr, DatabaseURL, JWTSecret              string
	JWTTTLHours                                   int
	RedisURL, RedisKeyPrefix                      string
	UploadDedupTTL                                time.Duration
	UploadRateLimitCapacity                       int
	UploadRateLimitRefillInterval                 time.Duration
	UploadRateLimitKeyTTL                         time.Duration
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
	// Prepare 下载限制由 Worker 强制执行，避免外部地址长期占用连接、内存或磁盘。
	PrepareMaxFileSizeMiB                               int
	PrepareConnectTimeout, PrepareResponseHeaderTimeout time.Duration
	PrepareDownloadTimeout                              time.Duration
	PrepareMaxRedirects                                 int
	// yt-dlp 只处理经过白名单识别的 Bilibili 页面，格式与时长限制继续由 prepare 统一控制。
	YTDLPPath          string
	YTDLPMaxHeight     int
	PrepareMaxDuration time.Duration
}

const (
	// 以下参数属于服务内部策略，统一以内嵌常量维护，避免普通用户误改后破坏任务一致性。
	internalJWTTTLHours                  = 24
	internalRedisKeyPrefix               = "readergo"
	internalUploadDedupTTL               = 15 * time.Minute
	internalUploadRateLimitCapacity      = 5
	internalUploadRateLimitRefill        = 90 * time.Second
	internalUploadRateLimitKeyTTL        = 15 * time.Minute
	internalMinIOBucket                  = "videos"
	internalRabbitMQJobsExchange         = "readergo.video.jobs"
	internalRabbitMQDLXExchange          = "readergo.video.dlx"
	internalRabbitMQPrefetch             = 1
	internalRabbitMQConsumerTimeout      = 6 * time.Hour
	internalJobMaxAttempts               = 4
	internalJobLeaseTTL                  = 2 * time.Minute
	internalJobHeartbeatInterval         = 30 * time.Second
	internalOutboxPollInterval           = 500 * time.Millisecond
	internalOutboxBatchSize              = 100
	internalOCRSceneThreshold            = 0.3
	internalOCRHashDistance              = 5
	internalRAGRequestTimeout            = 2 * time.Minute
	internalChatRequestTimeout           = time.Minute
	internalChatMaxInputRunes            = 4000
	internalChatHistoryTurns             = 3
	internalChatRAGMinSimilarity         = 0.25
	internalIntentRequestTimeout         = 20 * time.Second
	internalIntentConfidenceThreshold    = 0.75
	internalUploadPartSizeMiB            = 32
	internalUploadURLTTLMinutes          = 15
	internalPrepareConnectTimeout        = 10 * time.Second
	internalPrepareResponseHeaderTimeout = 30 * time.Second
	internalPrepareDownloadTimeout       = 2 * time.Hour
	internalPrepareMaxRedirects          = 5
)

// Load 优先加载本地 .env，再校验生产环境必填配置。
func Load() (Config, error) {
	_ = godotenv.Load()
	prepareMaxFileSizeMiB, err := strictIntValue("PREPARE_MAX_FILE_SIZE_MIB", 2048)
	if err != nil {
		return Config{}, err
	}
	ytdlpMaxHeight, err := strictIntValue("YT_DLP_MAX_HEIGHT", 1080)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		HTTPAddr:                      value("HTTP_ADDR", ":8080"),
		DatabaseURL:                   os.Getenv("DATABASE_URL"),
		JWTSecret:                     os.Getenv("JWT_SECRET"),
		JWTTTLHours:                   internalJWTTTLHours,
		RedisURL:                      value("REDIS_URL", "redis://localhost:6379/0"),
		RedisKeyPrefix:                internalRedisKeyPrefix,
		UploadDedupTTL:                internalUploadDedupTTL,
		UploadRateLimitCapacity:       internalUploadRateLimitCapacity,
		UploadRateLimitRefillInterval: internalUploadRateLimitRefill,
		UploadRateLimitKeyTTL:         internalUploadRateLimitKeyTTL,

		MinIOEndpoint:  os.Getenv("MINIO_ENDPOINT"),
		MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"),
		MinIOBucket:    internalMinIOBucket,
		MinIOSecure:    boolValue("MINIO_SECURE", false),

		RabbitMQURL:             value("RABBITMQ_URL", "amqp://readergo:readergo-secret@localhost:5672/readergo"),
		RabbitMQJobsExchange:    internalRabbitMQJobsExchange,
		RabbitMQDLXExchange:     internalRabbitMQDLXExchange,
		RabbitMQPrefetch:        internalRabbitMQPrefetch,
		RabbitMQConsumerTimeout: internalRabbitMQConsumerTimeout,

		JobMaxAttempts:       internalJobMaxAttempts,
		JobRetryDelays:       []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute},
		JobLeaseTTL:          internalJobLeaseTTL,
		JobHeartbeatInterval: internalJobHeartbeatInterval,
		OutboxPollInterval:   internalOutboxPollInterval,
		OutboxBatchSize:      internalOutboxBatchSize,

		FFmpegPath:     value("FFMPEG_PATH", "ffmpeg"),
		ASRAPIKey:      os.Getenv("ASR_API_KEY"),
		ASRBaseURL:     value("ASR_BASE_URL", "https://api.xiaomimimo.com/v1"),
		ASRModel:       value("ASR_MODEL", "mimo-v2.5-asr"),
		ASRConcurrency: intValue("ASR_CONCURRENCY", 3),

		OCRAPIKey:         os.Getenv("OCR_API_KEY"),
		OCRBaseURL:        os.Getenv("OCR_BASE_URL"),
		OCRModel:          value("OCR_MODEL", "qwen-vl-ocr"),
		OCRSceneThreshold: internalOCRSceneThreshold,
		OCRHashDistance:   internalOCRHashDistance,
		OCRConcurrency:    intValue("OCR_CONCURRENCY", 5),

		RAGAPIKey:         os.Getenv("RAG_API_KEY"),
		RAGBaseURL:        value("RAG_BASE_URL", "https://api.openai.com/v1"),
		RAGChatModel:      value("RAG_CHAT_MODEL", "gpt-4.1-mini"),
		RAGEmbeddingModel: value("RAG_EMBEDDING_MODEL", "text-embedding-3-small"),
		RAGConcurrency:    intValue("RAG_CONCURRENCY", 10),
		RAGRequestTimeout: internalRAGRequestTimeout,

		ChatAPIKey:           os.Getenv("CHAT_API_KEY"),
		ChatBaseURL:          value("CHAT_BASE_URL", "https://api.openai.com/v1"),
		ChatModel:            value("CHAT_MODEL", "gpt-4.1-mini"),
		ChatRequestTimeout:   internalChatRequestTimeout,
		ChatRAGMinSimilarity: internalChatRAGMinSimilarity,
		ChatMaxInputRunes:    internalChatMaxInputRunes,
		ChatHistoryTurns:     internalChatHistoryTurns,

		IntentAPIKey:              os.Getenv("INTENT_API_KEY"),
		IntentBaseURL:             value("INTENT_BASE_URL", "https://api.openai.com/v1"),
		IntentModel:               value("INTENT_MODEL", "gpt-4.1-mini"),
		IntentRequestTimeout:      internalIntentRequestTimeout,
		IntentConfidenceThreshold: internalIntentConfidenceThreshold,

		UploadPartSizeMiB:   internalUploadPartSizeMiB,
		UploadURLTTLMinutes: internalUploadURLTTLMinutes,

		PrepareMaxFileSizeMiB:        prepareMaxFileSizeMiB,
		PrepareConnectTimeout:        internalPrepareConnectTimeout,
		PrepareResponseHeaderTimeout: internalPrepareResponseHeaderTimeout,
		PrepareDownloadTimeout:       internalPrepareDownloadTimeout,
		PrepareMaxRedirects:          internalPrepareMaxRedirects,
		PrepareMaxDuration:           durationValue("PREPARE_MAX_DURATION", 6*time.Hour),
		YTDLPPath:                    value("YT_DLP_PATH", "yt-dlp"),
		YTDLPMaxHeight:               ytdlpMaxHeight,
	}
	if c.DatabaseURL == "" || len(c.JWTSecret) < 32 || c.MinIOEndpoint == "" || c.MinIOAccessKey == "" || c.MinIOSecretKey == "" {
		return Config{}, fmt.Errorf("DATABASE_URL, MinIO credentials, and a JWT_SECRET of at least 32 characters are required")
	}
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		return Config{}, fmt.Errorf("REDIS_URL must be a valid redis URL: %w", err)
	}
	parsedRabbitURL, err := url.Parse(c.RabbitMQURL)
	if err != nil || (parsedRabbitURL.Scheme != "amqp" && parsedRabbitURL.Scheme != "amqps") || parsedRabbitURL.Host == "" {
		return Config{}, fmt.Errorf("RABBITMQ_URL must be a valid amqp or amqps URL")
	}
	if c.ASRConcurrency < 1 || c.ASRConcurrency > 10 {
		return Config{}, fmt.Errorf("ASR_CONCURRENCY must be between 1 and 10")
	}
	if c.OCRConcurrency < 1 || c.OCRConcurrency > 10 {
		return Config{}, fmt.Errorf("OCR_CONCURRENCY must be between 1 and 10")
	}
	if c.RAGConcurrency < 1 || c.RAGConcurrency > 10 {
		return Config{}, fmt.Errorf("RAG_CONCURRENCY must be between 1 and 10")
	}
	if c.PrepareMaxFileSizeMiB < 1 || c.PrepareMaxFileSizeMiB > 10240 {
		return Config{}, fmt.Errorf("PREPARE_MAX_FILE_SIZE_MIB must be between 1 and 10240")
	}
	if c.PrepareMaxDuration <= 0 || c.PrepareMaxDuration > 24*time.Hour {
		return Config{}, fmt.Errorf("PREPARE_MAX_DURATION must be positive and at most 24h")
	}
	if strings.TrimSpace(c.YTDLPPath) == "" {
		return Config{}, fmt.Errorf("YT_DLP_PATH must not be empty")
	}
	if c.YTDLPMaxHeight < 144 || c.YTDLPMaxHeight > 4320 {
		return Config{}, fmt.Errorf("YT_DLP_MAX_HEIGHT must be between 144 and 4320")
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

func durationValue(k string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(value(k, fallback.String()))
	if err != nil {
		return 0
	}
	return d
}
