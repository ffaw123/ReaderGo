package main

import (
	"context"
	"log"
	"readergo/internal/api"
	"readergo/internal/auth"
	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/queue"
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
	// 任务生产者在启动阶段创建，避免接收请求后才发现 Kafka 不可用。
	q, e := queue.NewProducer(c.Brokers())
	if e != nil {
		log.Fatal(e)
	}
	defer q.Close()
	uploads, e := storage.NewMultipartStore(c.MinIOEndpoint, c.MinIOAccessKey, c.MinIOSecretKey, c.MinIOBucket, c.MinIOSecure, time.Duration(c.UploadURLTTLMinutes)*time.Minute)
	if e != nil {
		log.Fatal(e)
	}
	if e = uploads.EnsureBucket(context.Background()); e != nil {
		log.Fatal(e)
	}
	// HTTP 服务仅处理同步请求；耗时视频处理通过 Kafka 交给 Worker。
	log.Fatal(api.New(d, auth.New(c.JWTSecret, c.JWTTTLHours), q, uploads, int64(c.UploadPartSizeMiB)*1024*1024).Router().Run(c.HTTPAddr))
}
