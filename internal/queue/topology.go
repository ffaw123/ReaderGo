package queue

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type TopologyConfig struct {
	JobsExchange    string
	DLXExchange     string
	ConsumerTimeout time.Duration
}

type QueueSpec struct {
	TaskType   string
	RoutingKey string
	QueueName  string
	DLQName    string
}

var QueueSpecs = []QueueSpec{
	{TaskType: "prepare", RoutingKey: RoutingKeyPrepare, QueueName: "readergo.video.prepare", DLQName: "readergo.video.prepare.dlq"},
	{TaskType: "asr", RoutingKey: RoutingKeyASR, QueueName: "readergo.video.asr", DLQName: "readergo.video.asr.dlq"},
	{TaskType: "vision", RoutingKey: RoutingKeyVision, QueueName: "readergo.video.vision", DLQName: "readergo.video.vision.dlq"},
	{TaskType: "rag_index", RoutingKey: RoutingKeyRAGIndex, QueueName: "readergo.video.rag-index", DLQName: "readergo.video.rag-index.dlq"},
	{TaskType: "embedding", RoutingKey: RoutingKeyEmbedding, QueueName: "readergo.video.embedding", DLQName: "readergo.video.embedding.dlq"},
	{TaskType: "summary", RoutingKey: RoutingKeySummary, QueueName: "readergo.video.summary", DLQName: "readergo.video.summary.dlq"},
}

func DeclareTopology(channel *amqp.Channel, config TopologyConfig) error {
	if config.JobsExchange == "" || config.DLXExchange == "" || config.ConsumerTimeout <= 0 {
		return fmt.Errorf("invalid RabbitMQ topology configuration")
	}
	if err := channel.ExchangeDeclare(config.JobsExchange, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare jobs exchange: %w", err)
	}
	if err := channel.ExchangeDeclare(config.DLXExchange, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead-letter exchange: %w", err)
	}
	for _, spec := range QueueSpecs {
		// 主队列和死信队列均使用 durable quorum queue，使单节点开发和生产拓扑保持一致。
		mainArgs := amqp.Table{
			"x-queue-type":              "quorum",
			"x-dead-letter-exchange":    config.DLXExchange,
			"x-dead-letter-routing-key": spec.RoutingKey,
			"x-consumer-timeout":        config.ConsumerTimeout.Milliseconds(),
		}
		if _, err := channel.QueueDeclare(spec.QueueName, true, false, false, false, mainArgs); err != nil {
			return fmt.Errorf("declare queue %s: %w", spec.QueueName, err)
		}
		if err := channel.QueueBind(spec.QueueName, spec.RoutingKey, config.JobsExchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %s: %w", spec.QueueName, err)
		}
		if _, err := channel.QueueDeclare(spec.DLQName, true, false, false, false, amqp.Table{"x-queue-type": "quorum"}); err != nil {
			return fmt.Errorf("declare DLQ %s: %w", spec.DLQName, err)
		}
		if err := channel.QueueBind(spec.DLQName, spec.RoutingKey, config.DLXExchange, false, nil); err != nil {
			return fmt.Errorf("bind DLQ %s: %w", spec.DLQName, err)
		}
	}
	return nil
}
