package api

import (
	"context"
	"errors"
	"sync"

	"readergo/internal/visionagent"

	"github.com/google/uuid"
)

var (
	errRangeConfirmationNotFound = errors.New("vision range confirmation not found")
	errRangeConfirmationMismatch = errors.New("vision range confirmation does not match the suggestion")
)

type pendingRangeConfirmation struct {
	id             string
	tenantID       string
	conversationID string
	suggested      visionagent.Range
	confirmed      chan visionagent.Range
}

type rangeConfirmationBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingRangeConfirmation
}

func newRangeConfirmationBroker() *rangeConfirmationBroker {
	return &rangeConfirmationBroker{pending: make(map[string]*pendingRangeConfirmation)}
}

func (b *rangeConfirmationBroker) register(tenantID, conversationID string, suggested visionagent.Range) *pendingRangeConfirmation {
	pending := &pendingRangeConfirmation{
		id: uuid.NewString(), tenantID: tenantID, conversationID: conversationID,
		suggested: suggested, confirmed: make(chan visionagent.Range, 1),
	}
	b.mu.Lock()
	b.pending[pending.id] = pending
	b.mu.Unlock()
	return pending
}

func (b *rangeConfirmationBroker) wait(ctx context.Context, pending *pendingRangeConfirmation) (visionagent.Range, error) {
	defer b.cancel(pending.id)
	select {
	case value := <-pending.confirmed:
		return value, nil
	case <-ctx.Done():
		// 确认和连接取消可能同时发生；已到达的确认优先，避免接口成功但原请求误判失败。
		select {
		case value := <-pending.confirmed:
			return value, nil
		default:
			return visionagent.Range{}, ctx.Err()
		}
	}
}

func (b *rangeConfirmationBroker) cancel(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

func (b *rangeConfirmationBroker) confirm(id, tenantID, conversationID string, value visionagent.Range) error {
	b.mu.Lock()
	pending, ok := b.pending[id]
	if !ok || pending.tenantID != tenantID || pending.conversationID != conversationID {
		b.mu.Unlock()
		return errRangeConfirmationNotFound
	}
	if pending.suggested != value {
		b.mu.Unlock()
		return errRangeConfirmationMismatch
	}
	delete(b.pending, id)
	b.mu.Unlock()

	// 通道容量为 1，确认接口不会被仍在写 SSE 的 Agent goroutine 阻塞。
	pending.confirmed <- value
	return nil
}
