package main

import (
	"context"
	"github.com/IBM/sarama"
	"log"
	"os"
	"os/signal"
	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/queue"
	"readergo/internal/worker"
	"syscall"
)

func main() {
	log.Println("readergo worker starting ...")

	// Worker 复用与 API 一致的配置和数据库，以便更新同一份任务状态。
	c, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	log.Printf("config loaded, brokers=%v", c.Brokers())

	d, e := db.Open(c.DatabaseURL)
	if e != nil {
		log.Fatal(e)
	}
	log.Println("database connected")

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
		if e := group.Consume(ctx, queue.VideoJobsTopics, worker.New(d, worker.PlaceholderProcessor{}, jobs)); e != nil && ctx.Err() == nil {
			log.Printf("consumer error: %v", e)
		}
	}

	log.Println("worker shutting down")
}
