package visionagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalLimiterEnforcesTenantConcurrency(t *testing.T) {
	limiter := NewLocalLimiter(1, 10, time.Minute)
	release, err := limiter.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(context.Background(), "tenant-1"); !errors.Is(err, ErrConcurrentLimit) {
		t.Fatalf("second concurrent request error = %v", err)
	}
	// 不同租户拥有独立额度。
	otherRelease, err := limiter.Acquire(context.Background(), "tenant-2")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()
	release()
}

func TestLocalLimiterEnforcesRequestRate(t *testing.T) {
	limiter := NewLocalLimiter(1, 1, time.Minute)
	release, err := limiter.Acquire(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := limiter.Acquire(context.Background(), "tenant-1"); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("second request error = %v, want ErrRateLimit", err)
	}
}
