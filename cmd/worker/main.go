package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/media"
	"readergo/internal/outbox"
	"readergo/internal/queue"
	"readergo/internal/worker"

	"github.com/google/uuid"
)

func main() {
	log.Println("readergo worker starting ...")
	configValue, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// Worker 启动时先验证媒体工具，避免领取 RabbitMQ 任务后才发现 FFmpeg 不可用。
	ffmpeg, err := media.NewFFmpeg(configValue.FFmpegPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := ffmpeg.Validate(context.Background()); err != nil {
		log.Fatal(err)
	}
	database, err := db.Open(configValue.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	prepareProcessor, err := worker.NewPrepareProcessor(database, ffmpeg, configValue.MinIOEndpoint, configValue.MinIOAccessKey, configValue.MinIOSecretKey, configValue.MinIOSecure, configValue.MinIOBucket, worker.PrepareOptions{
		MaxFileSize:           int64(configValue.PrepareMaxFileSizeMiB) * 1024 * 1024,
		MaxDuration:           configValue.PrepareMaxDuration,
		ConnectTimeout:        configValue.PrepareConnectTimeout,
		ResponseHeaderTimeout: configValue.PrepareResponseHeaderTimeout,
		DownloadTimeout:       configValue.PrepareDownloadTimeout,
		MaxRedirects:          configValue.PrepareMaxRedirects,
		YTDLPPath:             configValue.YTDLPPath,
		YTDLPMaxHeight:        configValue.YTDLPMaxHeight,
	})
	if err != nil {
		log.Fatal(err)
	}
	asrProcessor, err := worker.NewASRProcessor(database, ffmpeg, configValue.MinIOEndpoint, configValue.MinIOAccessKey, configValue.MinIOSecretKey, configValue.MinIOSecure, configValue.MinIOBucket, configValue.ASRBaseURL, configValue.ASRAPIKey, configValue.ASRModel, configValue.ASRConcurrency)
	if err != nil {
		log.Fatal(err)
	}
	ocrProcessor, err := worker.NewOCRProcessor(database, ffmpeg, configValue.MinIOEndpoint, configValue.MinIOAccessKey, configValue.MinIOSecretKey, configValue.MinIOSecure, configValue.MinIOBucket, configValue.OCRBaseURL, configValue.OCRAPIKey, configValue.OCRModel, configValue.OCRSceneThreshold, configValue.OCRHashDistance, configValue.OCRConcurrency)
	if err != nil {
		log.Fatal(err)
	}
	ragIndexProcessor, err := worker.NewRAGIndexProcessor(database, configValue.RAGBaseURL, configValue.RAGAPIKey, configValue.RAGChatModel, configValue.RAGConcurrency, configValue.RAGRequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	embeddingProcessor, err := worker.NewEmbeddingProcessor(database, configValue.RAGBaseURL, configValue.RAGAPIKey, configValue.RAGEmbeddingModel, configValue.RAGRequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	summaryProcessor, err := worker.NewSummaryProcessor(database, configValue.RAGBaseURL, configValue.RAGAPIKey, configValue.RAGChatModel, configValue.RAGRequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	writer := outbox.NewWriter(configValue.RabbitMQJobsExchange, configValue.RabbitMQDLXExchange)
	// 创建一个worker实例
	handler := worker.New(database, worker.NewRouterProcessor(prepareProcessor, asrProcessor, ocrProcessor, ragIndexProcessor, embeddingProcessor, summaryProcessor), writer, worker.Options{
		WorkerID: "worker-" + uuid.NewString(), MaxAttempts: configValue.JobMaxAttempts,
		RetryDelays: configValue.JobRetryDelays, LeaseTTL: configValue.JobLeaseTTL,
		HeartbeatInterval: configValue.JobHeartbeatInterval,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 租约回收独立于消息消费，RabbitMQ/Worker 重启后 running 任务仍能回到 pending 并重新派发。
	go handler.RunReconciler(ctx, configValue.JobHeartbeatInterval)
	topology := queue.TopologyConfig{
		JobsExchange: configValue.RabbitMQJobsExchange, DLXExchange: configValue.RabbitMQDLXExchange,
		ConsumerTimeout: configValue.RabbitMQConsumerTimeout,
	}
	log.Printf("worker ready, consuming %d RabbitMQ queues", len(queue.QueueSpecs))
	if err := queue.RunConsumers(ctx, queue.ConsumerConfig{
		URL: configValue.RabbitMQURL, Topology: topology, Prefetch: configValue.RabbitMQPrefetch,
	}, handler); err != nil {
		log.Fatal(err)
	}
	log.Println("worker shutting down")
}
