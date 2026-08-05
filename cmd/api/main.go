package main

import (
	"context"
	"log"
	"readergo/internal/api"
	"readergo/internal/auth"
	"readergo/internal/chat"
	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/outbox"
	"readergo/internal/storage"
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
	uploads, e := storage.NewMultipartStore(c.MinIOEndpoint, c.MinIOAccessKey, c.MinIOSecretKey, c.MinIOBucket, c.MinIOSecure, time.Duration(c.UploadURLTTLMinutes)*time.Minute)
	if e != nil {
		log.Fatal(e)
	}
	if e = uploads.EnsureBucket(context.Background()); e != nil {
		log.Fatal(e)
	}
	// 用户回答模型、内部意图模型与 RAG 查询向量服务分别使用独立配置。
	chatService, e := chat.NewService(d, chat.ServiceConfig{
		ChatAPIKey: c.ChatAPIKey, ChatBaseURL: c.ChatBaseURL, ChatModel: c.ChatModel, ChatRequestTimeout: c.ChatRequestTimeout,
		IntentAPIKey: c.IntentAPIKey, IntentBaseURL: c.IntentBaseURL, IntentModel: c.IntentModel, IntentRequestTimeout: c.IntentRequestTimeout,
		IntentConfidenceThreshold: c.IntentConfidenceThreshold,
		EmbeddingAPIKey:           c.RAGAPIKey, EmbeddingBaseURL: c.RAGBaseURL, EmbeddingModel: c.RAGEmbeddingModel, EmbeddingRequestTimeout: c.RAGRequestTimeout,
		RAGMinSimilarity: c.ChatRAGMinSimilarity, MaxInputRunes: c.ChatMaxInputRunes, HistoryTurns: c.ChatHistoryTurns,
	})
	if e != nil {
		log.Fatal(e)
	}
	// 视频任务仍只写入 Outbox；同步对话则由 chatService 在当前请求中完成。
	writer := outbox.NewWriter(c.RabbitMQJobsExchange, c.RabbitMQDLXExchange)
	log.Fatal(api.New(d, auth.New(c.JWTSecret, c.JWTTTLHours), chatService, writer, uploads, int64(c.UploadPartSizeMiB)*1024*1024).Router().Run(c.HTTPAddr))
}
