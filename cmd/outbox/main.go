package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/outbox"
	"readergo/internal/queue"
)

func main() {
	configValue, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	database, err := db.Open(configValue.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	topology := queue.TopologyConfig{
		JobsExchange: configValue.RabbitMQJobsExchange, DLXExchange: configValue.RabbitMQDLXExchange,
		ConsumerTimeout: configValue.RabbitMQConsumerTimeout,
	}
	publisher, err := queue.NewRabbitPublisher(configValue.RabbitMQURL, topology)
	if err != nil {
		log.Fatal(err)
	}
	defer publisher.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Println("outbox dispatcher ready")
	if err := outbox.NewDispatcher(database, publisher, configValue.OutboxPollInterval, configValue.OutboxBatchSize).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
