package api

import (
	"context"
	"errors"
	"testing"

	"readergo/internal/visionagent"
)

func TestRangeConfirmationBrokerRequiresOwnerAndExactRange(t *testing.T) {
	broker := newRangeConfirmationBroker()
	suggested := visionagent.Range{StartMS: 18_000, EndMS: 28_000}
	pending := broker.register("tenant-1", "conversation-1", suggested)

	if err := broker.confirm(pending.id, "tenant-2", "conversation-1", suggested); !errors.Is(err, errRangeConfirmationNotFound) {
		t.Fatalf("cross-tenant confirmation error = %v", err)
	}
	if err := broker.confirm(pending.id, "tenant-1", "conversation-1", visionagent.Range{StartMS: 19_000, EndMS: 28_000}); !errors.Is(err, errRangeConfirmationMismatch) {
		t.Fatalf("mismatched confirmation error = %v", err)
	}
	if err := broker.confirm(pending.id, "tenant-1", "conversation-1", suggested); err != nil {
		t.Fatalf("confirm() error = %v", err)
	}
	confirmed, err := broker.wait(context.Background(), pending)
	if err != nil || confirmed != suggested {
		t.Fatalf("wait() = %#v, %v", confirmed, err)
	}
	if err := broker.confirm(pending.id, "tenant-1", "conversation-1", suggested); !errors.Is(err, errRangeConfirmationNotFound) {
		t.Fatalf("repeated confirmation error = %v", err)
	}
}

func TestRangeConfirmationBrokerRemovesCancelledWait(t *testing.T) {
	broker := newRangeConfirmationBroker()
	pending := broker.register("tenant-1", "conversation-1", visionagent.Range{StartMS: 0, EndMS: 10_000})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := broker.wait(ctx, pending); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait() error = %v", err)
	}
	if err := broker.confirm(pending.id, "tenant-1", "conversation-1", pending.suggested); !errors.Is(err, errRangeConfirmationNotFound) {
		t.Fatalf("cancelled confirmation error = %v", err)
	}
}
