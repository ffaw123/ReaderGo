package main

import (
	"context"
	"log"
	"readergo/internal/api"
	"readergo/internal/auth"
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
	// API 只原子写入任务和 Outbox，网络发布由独立 Dispatcher 完成。
	writer := outbox.NewWriter(c.RabbitMQJobsExchange, c.RabbitMQDLXExchange)
	log.Fatal(api.New(d, auth.New(c.JWTSecret, c.JWTTTLHours), writer, uploads, int64(c.UploadPartSizeMiB)*1024*1024).Router().Run(c.HTTPAddr))
}
