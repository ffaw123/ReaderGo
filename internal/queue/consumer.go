package queue

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type ConsumerConfig struct {
	URL      string
	Topology TopologyConfig
	Prefetch int
}

// RunConsumers 为五个阶段各开一个 channel，并在连接中断后重建拓扑和消费循环。
func RunConsumers(ctx context.Context, config ConsumerConfig, handler Handler) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := consumeConnection(ctx, config, handler)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("RabbitMQ consumer connection ended: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func consumeConnection(ctx context.Context, config ConsumerConfig, handler Handler) error {
	connection, err := amqp.Dial(config.URL)
	if err != nil {
		return fmt.Errorf("connect RabbitMQ: %w", err)
	}
	defer connection.Close()
	topologyChannel, err := connection.Channel()
	if err != nil {
		return err
	}
	if err := DeclareTopology(topologyChannel, config.Topology); err != nil {
		_ = topologyChannel.Close()
		return err
	}
	_ = topologyChannel.Close()

	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, len(QueueSpecs))
	for _, spec := range QueueSpecs {
		spec := spec
		go func() { errors <- consumeQueue(connectionCtx, connection, config.Prefetch, spec, handler) }()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errors:
		cancel()
		return err
	case closeErr := <-connection.NotifyClose(make(chan *amqp.Error, 1)):
		cancel()
		return fmt.Errorf("RabbitMQ connection closed: %v", closeErr)
	}
}

func consumeQueue(ctx context.Context, connection *amqp.Connection, prefetch int, spec QueueSpec, handler Handler) error {
	channel, err := connection.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := channel.Qos(prefetch, 0, false); err != nil {
		return fmt.Errorf("set QoS for %s: %w", spec.QueueName, err)
	}
	tag := "readergo-" + spec.TaskType + "-" + uuid.NewString()
	deliveries, err := channel.ConsumeWithContext(ctx, spec.QueueName, tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", spec.QueueName, err)
	}
	for delivery := range deliveries {
		message, decodeErr := DecodeMessage(delivery.Body)
		if decodeErr != nil || message.TaskType != spec.TaskType {
			log.Printf("reject poison message from %s: %v", spec.QueueName, decodeErr)
			if err := delivery.Reject(false); err != nil {
				return err
			}
			continue
		}
		handleErr := handler.Handle(ctx, message)
		if handleErr == nil {
			if err := delivery.Ack(false); err != nil {
				return err
			}
			continue
		}
		if IsPermanent(handleErr) {
			log.Printf("reject permanent job %s: %v", message.JobID, handleErr)
			if err := delivery.Reject(false); err != nil {
				return err
			}
			continue
		}
		log.Printf("temporarily NACK job %s: %v", message.JobID, handleErr)
		if err := delivery.Nack(false, true); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("delivery channel for %s closed", spec.QueueName)
}
