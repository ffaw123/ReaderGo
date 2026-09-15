package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"readergo/internal/api"
	"readergo/internal/auth"
	"readergo/internal/cache"
	"readergo/internal/chat"
	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/media"
	"readergo/internal/outbox"
	"readergo/internal/storage"
	"readergo/internal/uploaddedupe"
	"readergo/internal/uploadratelimit"
	"readergo/internal/visionagent"
	"syscall"
	"time"
)

func main() {
	// 启动前校验数据库连接与 JWT 密钥等不可缺少的运行时配置。
	c, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	// API 与 Worker 使用相同的数据模型，因此统一从 PostgreSQL 建立连接。
	d, e := db.Open(c.DatabaseURL)
	if e != nil {
		log.Fatal(e)
	}
	redisClient, e := cache.NewRedis(context.Background(), c.RedisURL, c.RedisKeyPrefix)
	if e != nil {
		log.Fatal(e)
	}
	defer redisClient.Close()
	uploadDeduper := uploaddedupe.New(d, redisClient, c.UploadDedupTTL)
	uploadLimiter := uploadratelimit.New(redisClient, c.UploadRateLimitCapacity, c.UploadRateLimitRefillInterval, c.UploadRateLimitKeyTTL)
	uploads, e := storage.NewMultipartStore(c.MinIOEndpoint, c.MinIOAccessKey, c.MinIOSecretKey, c.MinIOBucket, c.MinIOSecure, time.Duration(c.UploadURLTTLMinutes)*time.Minute)
	if e != nil {
		log.Fatal(e)
	}
	if e = uploads.EnsureBucket(context.Background()); e != nil {
		log.Fatal(e)
	}
	var visionRunner visionagent.Runner
	if c.VisionConfigured() {
		// 服务端参数完整时提供能力；是否使用由用户每次在前端手动开启。
		ffmpeg, visionErr := media.NewFFmpeg(c.FFmpegPath)
		if visionErr != nil {
			log.Fatal(visionErr)
		}
		if visionErr = ffmpeg.Validate(context.Background()); visionErr != nil {
			log.Fatal(visionErr)
		}
		source, visionErr := visionagent.NewMinIOVideoSource(c.MinIOEndpoint, c.MinIOAccessKey, c.MinIOSecretKey, c.MinIOSecure, c.MinIOBucket)
		if visionErr != nil {
			log.Fatal(visionErr)
		}
		// 同时限制缓存条目和总磁盘占用，避免长视频耗尽 API 节点空间。
		cachedSource, visionErr := visionagent.NewCachedVideoSource(source, 4, 8<<30, 30*time.Minute)
		if visionErr != nil {
			log.Fatal(visionErr)
		}
		defer cachedSource.Close()
		visionRunner, visionErr = visionagent.New(d, cachedSource, ffmpeg, visionagent.Options{
			BaseURL: c.VisionBaseURL, APIKey: c.VisionAPIKey, Model: c.VisionModel,
			RequestTimeout: c.VisionRequestTimeout, TotalTimeout: c.VisionTotalTimeout,
		})
		if visionErr != nil {
			log.Fatal(visionErr)
		}
	}
	// 用户回答模型、内部意图模型与 RAG 查询向量服务分别使用独立配置。
	chatService, e := chat.NewService(d, chat.ServiceConfig{
		ChatAPIKey: c.ChatAPIKey, ChatBaseURL: c.ChatBaseURL, ChatModel: c.ChatModel, ChatRequestTimeout: c.ChatRequestTimeout,
		IntentAPIKey: c.IntentAPIKey, IntentBaseURL: c.IntentBaseURL, IntentModel: c.IntentModel, IntentRequestTimeout: c.IntentRequestTimeout,
		IntentConfidenceThreshold: c.IntentConfidenceThreshold,
		EmbeddingAPIKey:           c.RAGAPIKey, EmbeddingBaseURL: c.RAGBaseURL, EmbeddingModel: c.RAGEmbeddingModel, EmbeddingRequestTimeout: c.RAGRequestTimeout,
		RAGMinSimilarity: c.ChatRAGMinSimilarity, MaxInputRunes: c.ChatMaxInputRunes, HistoryTurns: c.ChatHistoryTurns,
		VisionRunner: visionRunner,
	})
	if e != nil {
		log.Fatal(e)
	}
	// 视频任务仍只写入 Outbox；同步对话则由 chatService 在当前请求中完成。
	writer := outbox.NewWriter(c.RabbitMQJobsExchange, c.RabbitMQDLXExchange)
	server := api.New(d, auth.New(c.JWTSecret, c.JWTTTLHours), chatService, writer, uploads, uploadDeduper, uploadLimiter, c.UploadRateLimitCapacity, int64(c.UploadPartSizeMiB)*1024*1024)
	cleanupCtx, stopCleanup := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopCleanup()
	// 过期会话会同时清理 MinIO 临时分片并保留数据库审计终态。
	go server.RunUploadCleanup(cleanupCtx, 15*time.Minute)
	log.Fatal(server.Router().Run(c.HTTPAddr))
}
