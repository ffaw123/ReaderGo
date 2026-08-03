package queue

import (
	"context"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitPublisher 串行化单个 confirm channel 上的发布，确保 confirm/return 与 Outbox 记录一一对应。
type RabbitPublisher struct {
	url      string
	topology TopologyConfig
	mu       sync.Mutex
	conn     *amqp.Connection
	channel  *amqp.Channel
	confirms <-chan amqp.Confirmation
	returns  <-chan amqp.Return
	closed   <-chan *amqp.Error
}

func NewRabbitPublisher(url string, topology TopologyConfig) (*RabbitPublisher, error) {
	// 连接采用惰性建立；RabbitMQ 暂时不可用时 Dispatcher 仍可启动并持续重试 Outbox。
	return &RabbitPublisher{url: url, topology: topology}, nil
}

func (p *RabbitPublisher) connectLocked() error {
	connection, err := amqp.Dial(p.url)
	if err != nil {
		return fmt.Errorf("connect RabbitMQ: %w", err)
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		return fmt.Errorf("open RabbitMQ channel: %w", err)
	}
	if err := DeclareTopology(channel, p.topology); err != nil {
		_ = channel.Close()
		_ = connection.Close()
		return err
	}
	if err := channel.Confirm(false); err != nil {
		_ = channel.Close()
		_ = connection.Close()
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	p.conn = connection
	p.channel = channel
	p.confirms = channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	p.returns = channel.NotifyReturn(make(chan amqp.Return, 1))
	p.closed = channel.NotifyClose(make(chan *amqp.Error, 1))
	return nil
}

func (p *RabbitPublisher) invalidateLocked() {
	if p.channel != nil {
		_ = p.channel.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.channel = nil
	p.conn = nil
}

func (p *RabbitPublisher) Publish(ctx context.Context, message PublishMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.channel == nil || p.channel.IsClosed() || p.conn == nil || p.conn.IsClosed() {
		p.invalidateLocked()
		if err := p.connectLocked(); err != nil {
			return err
		}
	}
	if message.MessageID == "" || message.Exchange == "" || message.RoutingKey == "" || len(message.Body) == 0 {
		return fmt.Errorf("publish message is incomplete")
	}
	if err := p.channel.PublishWithContext(ctx, message.Exchange, message.RoutingKey, true, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: message.MessageID,
		Timestamp: time.Now().UTC(), Body: message.Body,
	}); err != nil {
		p.invalidateLocked()
		return fmt.Errorf("publish RabbitMQ message: %w", err)
	}
	for {
		select {
		case returned := <-p.returns:
			p.invalidateLocked()
			return fmt.Errorf("RabbitMQ returned unroutable message %s: %s", message.MessageID, returned.ReplyText)
		case confirmation, open := <-p.confirms:
			if !open {
				p.invalidateLocked()
				return fmt.Errorf("RabbitMQ confirm channel closed")
			}
			if !confirmation.Ack {
				p.invalidateLocked()
				return fmt.Errorf("RabbitMQ NACKed message %s", message.MessageID)
			}
			// RabbitMQ 会在 confirm 前发送 mandatory return；此处再检查一次已缓冲的 return。
			select {
			case returned := <-p.returns:
				p.invalidateLocked()
				return fmt.Errorf("RabbitMQ returned unroutable message %s: %s", message.MessageID, returned.ReplyText)
			default:
				return nil
			}
		case closeErr := <-p.closed:
			p.invalidateLocked()
			return fmt.Errorf("RabbitMQ publish channel closed: %v", closeErr)
		case <-ctx.Done():
			// 超时后 confirm 可能迟到；关闭 channel 避免它被下一条发布误认领。
			p.invalidateLocked()
			return ctx.Err()
		}
	}
}

func (p *RabbitPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidateLocked()
	return nil
}
