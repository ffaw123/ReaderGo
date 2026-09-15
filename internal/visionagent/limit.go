package visionagent

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrConcurrentLimit = errors.New("another vision request is already running for this tenant")
	ErrRateLimit       = errors.New("vision request rate limit exceeded")
)

type Limiter interface {
	Acquire(context.Context, string) (func(), error)
}

type tenantLimitState struct {
	active   int
	requests []time.Time
}

// LocalLimiter 限制单个 API 进程内每租户的并发和请求速率。
type LocalLimiter struct {
	mu            sync.Mutex
	states        map[string]*tenantLimitState
	maxConcurrent int
	maxRequests   int
	window        time.Duration
}

func NewLocalLimiter(maxConcurrent, maxRequests int, window time.Duration) *LocalLimiter {
	return &LocalLimiter{states: make(map[string]*tenantLimitState), maxConcurrent: maxConcurrent, maxRequests: maxRequests, window: window}
}

func (l *LocalLimiter) Acquire(ctx context.Context, tenantID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	l.mu.Lock()
	state := l.states[tenantID]
	if state == nil {
		state = &tenantLimitState{}
		l.states[tenantID] = state
	}
	cutoff := now.Add(-l.window)
	kept := state.requests[:0]
	for _, requestAt := range state.requests {
		if requestAt.After(cutoff) {
			kept = append(kept, requestAt)
		}
	}
	state.requests = kept
	if state.active >= l.maxConcurrent {
		l.mu.Unlock()
		return nil, ErrConcurrentLimit
	}
	if len(state.requests) >= l.maxRequests {
		l.mu.Unlock()
		return nil, ErrRateLimit
	}
	state.active++
	state.requests = append(state.requests, now)
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if current := l.states[tenantID]; current != nil && current.active > 0 {
				current.active--
			}
		})
	}, nil
}
