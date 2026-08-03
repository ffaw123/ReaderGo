package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"readergo/internal/model"
	"readergo/internal/queue"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const publishTimeout = 15 * time.Second

type Dispatcher struct {
	db           *gorm.DB
	publisher    queue.Publisher
	pollInterval time.Duration
	batchSize    int
}

func NewDispatcher(db *gorm.DB, publisher queue.Publisher, pollInterval time.Duration, batchSize int) *Dispatcher {
	return &Dispatcher{db: db, publisher: publisher, pollInterval: pollInterval, batchSize: batchSize}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		if _, err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("outbox dispatch failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	records, err := d.claimBatch(ctx)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, record := range records {
		publishCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		err := d.publisher.Publish(publishCtx, queue.PublishMessage{
			MessageID: record.ID, Exchange: record.ExchangeName, RoutingKey: record.RoutingKey, Body: []byte(record.Payload),
		})
		cancel()
		if err != nil {
			if resetErr := d.resetPending(ctx, record.ID, err); resetErr != nil {
				return published, fmt.Errorf("publish %s: %v; reset outbox: %w", record.ID, err, resetErr)
			}
			continue
		}
		result := d.db.WithContext(ctx).Model(&model.OutboxMessage{}).
			Where("id = ? AND status = ?", record.ID, model.OutboxStatusPublishing).
			Updates(map[string]any{"status": model.OutboxStatusPublished, "published_at": time.Now().UTC(), "last_error": ""})
		if result.Error != nil {
			return published, result.Error
		}
		if result.RowsAffected != 1 {
			return published, fmt.Errorf("outbox %s lost publishing claim", record.ID)
		}
		published++
	}
	return published, nil
}

func (d *Dispatcher) claimBatch(ctx context.Context) ([]model.OutboxMessage, error) {
	var records []model.OutboxMessage
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Dispatcher 崩溃后 publishing 记录会由其他实例重新领取；重复发布由 Worker 幂等抢占吸收。
		staleBefore := time.Now().UTC().Add(-2 * publishTimeout)
		if err := tx.Model(&model.OutboxMessage{}).
			Where("status = ? AND updated_at < ?", model.OutboxStatusPublishing, staleBefore).
			Updates(map[string]any{"status": model.OutboxStatusPending, "last_error": "stale publishing claim recovered"}).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND available_at <= ?", model.OutboxStatusPending, time.Now().UTC()).
			Order("created_at").Limit(d.batchSize).Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		ids := make([]string, len(records))
		for i := range records {
			ids[i] = records[i].ID
		}
		return tx.Model(&model.OutboxMessage{}).Where("id IN ?", ids).
			Updates(map[string]any{"status": model.OutboxStatusPublishing}).Error
	})
	return records, err
}

func (d *Dispatcher) resetPending(ctx context.Context, id string, publishErr error) error {
	return d.db.WithContext(ctx).Model(&model.OutboxMessage{}).
		Where("id = ? AND status = ?", id, model.OutboxStatusPublishing).
		Updates(map[string]any{
			"status": model.OutboxStatusPending, "publish_attempts": gorm.Expr("publish_attempts + 1"),
			"last_error": publishErr.Error(), "available_at": time.Now().UTC().Add(time.Second),
		}).Error
}
