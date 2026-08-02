package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config 保存 API 与 Worker 共用的运行时配置。
// 密钥只从环境变量读取，避免写入源代码。
type Config struct {
	HTTPAddr, DatabaseURL, KafkaBrokers, JWTSecret string
	JWTTTLHours                                    int
	MinIOEndpoint, MinIOAccessKey, MinIOSecretKey  string
	MinIOBucket                                    string
	MinIOSecure                                    bool
	// FFmpegPath 是 FFmpeg 可执行文件路径；Worker 启动时会校验它及同目录的 ffprobe。
	FFmpegPath string
	// ASR 配置保持厂商无关，通过 OpenAI 兼容客户端调用 Chat Completions 接口。
	ASRAPIKey, ASRBaseURL, ASRModel string
	ASRConcurrency                  int
	// UploadPartSizeMiB 限制为 16-64 MiB，以平衡重试成本和请求数量。
	UploadPartSizeMiB, UploadURLTTLMinutes int
}

// Load 优先加载本地 .env，再校验生产环境必填配置。
func Load() (Config, error) {
	_ = godotenv.Load()
	c := Config{HTTPAddr: value("HTTP_ADDR", ":8080"), DatabaseURL: os.Getenv("DATABASE_URL"), KafkaBrokers: value("KAFKA_BROKERS", "localhost:9092"), JWTSecret: os.Getenv("JWT_SECRET"), JWTTTLHours: intValue("JWT_TTL_HOURS", 24), MinIOEndpoint: os.Getenv("MINIO_ENDPOINT"), MinIOAccessKey: os.Getenv("MINIO_ACCESS_KEY"), MinIOSecretKey: os.Getenv("MINIO_SECRET_KEY"), MinIOBucket: value("MINIO_BUCKET", "videos"), MinIOSecure: boolValue("MINIO_SECURE", false), FFmpegPath: value("FFMPEG_PATH", "ffmpeg"), ASRAPIKey: os.Getenv("ASR_API_KEY"), ASRBaseURL: value("ASR_BASE_URL", "https://api.xiaomimimo.com/v1"), ASRModel: value("ASR_MODEL", "mimo-v2.5-asr"), ASRConcurrency: intValue("ASR_CONCURRENCY", 3), UploadPartSizeMiB: intValue("UPLOAD_PART_SIZE_MIB", 32), UploadURLTTLMinutes: intValue("UPLOAD_URL_TTL_MINUTES", 15)}
	if c.DatabaseURL == "" || len(c.JWTSecret) < 32 || c.MinIOEndpoint == "" || c.MinIOAccessKey == "" || c.MinIOSecretKey == "" {
		return Config{}, fmt.Errorf("DATABASE_URL, MinIO credentials, and a JWT_SECRET of at least 32 characters are required")
	}
	if c.UploadPartSizeMiB < 16 || c.UploadPartSizeMiB > 64 {
		return Config{}, fmt.Errorf("UPLOAD_PART_SIZE_MIB must be between 16 and 64")
	}
	if c.ASRConcurrency < 1 || c.ASRConcurrency > 16 {
		return Config{}, fmt.Errorf("ASR_CONCURRENCY must be between 1 and 16")
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

// Brokers 将逗号分隔的环境变量转换为 Sarama 所需的 Broker 地址列表。
func (c Config) Brokers() []string { return strings.Split(c.KafkaBrokers, ",") }

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
