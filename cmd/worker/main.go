package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/media"
	"readergo/internal/queue"
	"readergo/internal/worker"
	"syscall"

	"github.com/IBM/sarama"
)

func main() {
	log.Println("readergo worker starting ...")

	// Worker 复用与 API 一致的配置和数据库，以便更新同一份任务状态。
	c, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	log.Printf("config loaded, brokers=%v", c.Brokers())
	// Worker 启动时尽早验证媒体工具，避免 Kafka 任务开始消费后才发现本机缺少 FFmpeg。
	ffmpeg, e := media.NewFFmpeg(c.FFmpegPath)
	if e != nil {
		log.Fatal(e)
	}
	if e := ffmpeg.Validate(context.Background()); e != nil {
		log.Fatal(e)
	}
	log.Printf("ffmpeg ready: %s", ffmpeg.Path())

	d, e := db.Open(c.DatabaseURL)
	if e != nil {
		log.Fatal(e)
	}
	log.Println("database connected")

	// 构建各个阶段 Processor；当前仅接入 ASR，后续依次创建 Vision / RAG / Summary。
	asrProcessor, e := worker.NewASRProcessor(d, ffmpeg, c.MinIOEndpoint, c.MinIOAccessKey, c.MinIOSecretKey, c.MinIOSecure, c.MinIOBucket, c.ASRBaseURL, c.ASRAPIKey, c.ASRModel, c.ASRConcurrency)
	if e != nil {
		log.Fatal(e)
	}
	router := worker.NewRouterProcessor(asrProcessor)

	// 这里创建的生产者是给worker用的，任务完成 → 投递下一阶段任务（流水线接力）。
	jobs, e := queue.NewProducer(c.Brokers())
	if e != nil {
		log.Fatal(e)
	}
	defer jobs.Close()
	log.Println("kafka producer connected")

	// 显式指定 Kafka 协议版本，并从最早的未提交位点开始消费。
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_9_0_0
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	group, e := sarama.NewConsumerGroup(c.Brokers(), "readergo-video-workers", cfg)
	if e != nil {
		log.Fatal(e)
	}
	defer group.Close()
	log.Printf("consumer group joined, topics=%v", queue.VideoJobsTopics)

	// 收到终止信号后取消消费循环，让 Consumer Group 正常释放分区。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println("worker ready, waiting for jobs ...")

	// Consumer Group 重平衡后需要再次调用 Consume，因此在未退出时循环运行。
	for ctx.Err() == nil {
		if e := group.Consume(ctx, queue.VideoJobsTopics, worker.New(d, router, jobs)); e != nil && ctx.Err() == nil {
			log.Printf("consumer error: %v", e)
		}
	}

	log.Println("worker shutting down")
}
