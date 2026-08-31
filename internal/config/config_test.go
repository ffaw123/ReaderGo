package config

import (
	"testing"
	"time"
)

func TestValidateUploadRateLimit(t *testing.T) {
	if err := validateUploadRateLimit(5, 90*time.Second, 15*time.Minute); err != nil {
		t.Fatalf("valid upload rate limit was rejected: %v", err)
	}
	tests := []struct {
		name     string
		capacity int
		refill   time.Duration
		ttl      time.Duration
	}{
		{name: "zero capacity", capacity: 0, refill: 90 * time.Second, ttl: 15 * time.Minute},
		{name: "short refill", capacity: 5, refill: 500 * time.Millisecond, ttl: 15 * time.Minute},
		{name: "ttl shorter than full refill", capacity: 5, refill: 90 * time.Second, ttl: 7 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateUploadRateLimit(test.capacity, test.refill, test.ttl); err == nil {
				t.Fatal("invalid upload rate limit was accepted")
			}
		})
	}
}
