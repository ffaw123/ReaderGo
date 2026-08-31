package config

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadUsesInternalPolicyValues(t *testing.T) {
	// 必填外部依赖使用测试值，避免本机 .env 是否存在影响测试结果。
	required := map[string]string{
		"DATABASE_URL":     "postgres://readergo:readergo@localhost:5432/readergo?sslmode=disable",
		"JWT_SECRET":       "12345678901234567890123456789012",
		"MINIO_ENDPOINT":   "localhost:9000",
		"MINIO_ACCESS_KEY": "readergo",
		"MINIO_SECRET_KEY": "readergo",
		"REDIS_URL":        "redis://localhost:6379/0",
		"RABBITMQ_URL":     "amqp://readergo:readergo@localhost:5672/readergo",
	}
	for key, value := range required {
		t.Setenv(key, value)
	}

	// 即使旧环境变量仍残留，内部策略也必须以代码常量为准。
	overrides := map[string]string{
		"JWT_TTL_HOURS":                     "1",
		"REDIS_KEY_PREFIX":                  "overridden",
		"UPLOAD_DEDUP_TTL":                  "1m",
		"UPLOAD_RATE_LIMIT_CAPACITY":        "99",
		"UPLOAD_RATE_LIMIT_REFILL_INTERVAL": "1s",
		"UPLOAD_RATE_LIMIT_KEY_TTL":         "1h",
		"RABBITMQ_JOBS_EXCHANGE":            "overridden.jobs",
		"RABBITMQ_DLX_EXCHANGE":             "overridden.dlx",
		"RABBITMQ_PREFETCH":                 "99",
		"RABBITMQ_CONSUMER_TIMEOUT_MS":      "1000",
		"JOB_MAX_ATTEMPTS":                  "2",
		"JOB_RETRY_DELAYS":                  "1s",
		"JOB_LEASE_TTL":                     "1m",
		"JOB_HEARTBEAT_INTERVAL":            "1s",
		"OUTBOX_POLL_INTERVAL_MS":           "1000",
		"OUTBOX_BATCH_SIZE":                 "1",
		"MINIO_BUCKET":                      "overridden",
		"OCR_SCENE_THRESHOLD":               "0.9",
		"OCR_HASH_DISTANCE":                 "64",
		"RAG_REQUEST_TIMEOUT":               "1s",
		"CHAT_REQUEST_TIMEOUT":              "1s",
		"CHAT_MAX_INPUT_RUNES":              "1",
		"CHAT_HISTORY_TURNS":                "1",
		"CHAT_RAG_MIN_SIMILARITY":           "0.9",
		"INTENT_REQUEST_TIMEOUT":            "1s",
		"INTENT_CONFIDENCE_THRESHOLD":       "0.9",
		"UPLOAD_PART_SIZE_MIB":              "64",
		"UPLOAD_URL_TTL_MINUTES":            "1",
		"PREPARE_CONNECT_TIMEOUT":           "1s",
		"PREPARE_RESPONSE_HEADER_TIMEOUT":   "1s",
		"PREPARE_DOWNLOAD_TIMEOUT":          "1s",
		"PREPARE_MAX_REDIRECTS":             "1",
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}
	// 留空时应回退到当前 .env 已确认的推荐并发值。
	t.Setenv("ASR_CONCURRENCY", "")
	t.Setenv("OCR_CONCURRENCY", "")

	configValue, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if configValue.JWTTTLHours != 24 || configValue.RedisKeyPrefix != "readergo" || configValue.MinIOBucket != "videos" {
		t.Fatal("固定基础配置被环境变量覆盖")
	}
	if configValue.UploadDedupTTL != 15*time.Minute || configValue.UploadRateLimitCapacity != 5 || configValue.UploadRateLimitRefillInterval != 90*time.Second || configValue.UploadRateLimitKeyTTL != 15*time.Minute {
		t.Fatal("固定上传策略不符合预期")
	}
	if configValue.RabbitMQJobsExchange != "readergo.video.jobs" || configValue.RabbitMQDLXExchange != "readergo.video.dlx" || configValue.RabbitMQPrefetch != 1 || configValue.RabbitMQConsumerTimeout != 6*time.Hour {
		t.Fatal("固定 RabbitMQ 策略不符合预期")
	}
	if configValue.JobMaxAttempts != 4 || !reflect.DeepEqual(configValue.JobRetryDelays, []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute}) || configValue.JobLeaseTTL != 2*time.Minute || configValue.JobHeartbeatInterval != 30*time.Second {
		t.Fatal("固定任务策略不符合预期")
	}
	if configValue.OutboxPollInterval != 500*time.Millisecond || configValue.OutboxBatchSize != 100 {
		t.Fatal("固定 Outbox 策略不符合预期")
	}
	if configValue.OCRSceneThreshold != 0.3 || configValue.OCRHashDistance != 5 || configValue.RAGRequestTimeout != 2*time.Minute {
		t.Fatal("固定模型处理策略不符合预期")
	}
	if configValue.ASRConcurrency != 3 || configValue.OCRConcurrency != 5 {
		t.Fatal("ASR 或 OCR 默认并发数未与精简前的 .env 对齐")
	}
	if configValue.ChatRequestTimeout != time.Minute || configValue.ChatMaxInputRunes != 4000 || configValue.ChatHistoryTurns != 3 || configValue.ChatRAGMinSimilarity != 0.25 {
		t.Fatal("固定 Chat 策略不符合预期")
	}
	if configValue.IntentRequestTimeout != 20*time.Second || configValue.IntentConfidenceThreshold != 0.75 {
		t.Fatal("固定 Intent 策略不符合预期")
	}
	if configValue.UploadPartSizeMiB != 32 || configValue.UploadURLTTLMinutes != 15 || configValue.PrepareConnectTimeout != 10*time.Second || configValue.PrepareResponseHeaderTimeout != 30*time.Second || configValue.PrepareDownloadTimeout != 2*time.Hour || configValue.PrepareMaxRedirects != 5 {
		t.Fatal("固定上传与下载策略不符合预期")
	}
}
