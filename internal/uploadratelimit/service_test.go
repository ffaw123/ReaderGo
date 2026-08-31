package uploadratelimit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"readergo/internal/cache"

	miniredis "github.com/alicebob/miniredis/v2"
)

func newTestService(t *testing.T) (*Service, *cache.Client, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client, err := cache.NewRedis(context.Background(), "redis://"+server.Addr()+"/0", "readergo-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return New(client, 5, 90*time.Second, 15*time.Minute), client, server
}

func TestAllowConsumesBurstAndReturnsRetryAfter(t *testing.T) {
	service, client, server := newTestService(t)
	for request := 1; request <= 5; request++ {
		result, err := service.Allow(context.Background(), "tenant-1")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed || result.Remaining != 5-request {
			t.Fatalf("request %d result = %+v", request, result)
		}
	}

	result, err := service.Allow(context.Background(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed || result.Remaining != 0 || result.RetryAfter <= 0 || result.RetryAfter > 90*time.Second {
		t.Fatalf("limited result = %+v", result)
	}
	key := client.Key("rate-limit", "upload", "v1", "tenant-1")
	if ttl := server.TTL(key); ttl != 15*time.Minute {
		t.Fatalf("ttl = %v, want 15m", ttl)
	}
}

func TestAllowRefillsOneTokenAfterNinetySeconds(t *testing.T) {
	service, _, _ := newTestService(t)
	start := int64(1_788_166_800_000)
	for request := 0; request < 5; request++ {
		if result, err := service.allow(context.Background(), "tenant-1", &start); err != nil || !result.Allowed {
			t.Fatalf("initial request %d = (%+v, %v)", request+1, result, err)
		}
	}
	beforeRefill := start + (89 * time.Second).Milliseconds()
	if result, err := service.allow(context.Background(), "tenant-1", &beforeRefill); err != nil || result.Allowed {
		t.Fatalf("request before refill = (%+v, %v)", result, err)
	}
	atRefill := start + (90 * time.Second).Milliseconds()
	if result, err := service.allow(context.Background(), "tenant-1", &atRefill); err != nil || !result.Allowed || result.Remaining != 0 {
		t.Fatalf("request after refill = (%+v, %v)", result, err)
	}
	if result, err := service.allow(context.Background(), "tenant-1", &atRefill); err != nil || result.Allowed {
		t.Fatalf("second request after one refill = (%+v, %v)", result, err)
	}
}

func TestAllowUsesIndependentTenantBuckets(t *testing.T) {
	service, _, _ := newTestService(t)
	for request := 0; request < 5; request++ {
		_, _ = service.Allow(context.Background(), "tenant-1")
	}
	result, err := service.Allow(context.Background(), "tenant-2")
	if err != nil || !result.Allowed || result.Remaining != 4 {
		t.Fatalf("second tenant result = (%+v, %v)", result, err)
	}
}

func TestAllowIsAtomicUnderConcurrency(t *testing.T) {
	service, _, _ := newTestService(t)
	var allowed atomic.Int32
	errorsFound := make(chan error, 20)
	var group sync.WaitGroup
	for request := 0; request < 20; request++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := service.Allow(context.Background(), "tenant-1")
			if err != nil {
				errorsFound <- err
				return
			}
			if result.Allowed {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if got := allowed.Load(); got != 5 {
		t.Fatalf("allowed requests = %d, want 5", got)
	}
}

type fakeScriptStore struct {
	result any
	err    error
}

func (*fakeScriptStore) Key(parts ...string) string { return "test:" + strings.Join(parts, ":") }
func (s *fakeScriptStore) Eval(context.Context, string, []string, ...any) (any, error) {
	return s.result, s.err
}

func TestAllowRejectsRedisAndMalformedResults(t *testing.T) {
	service := New(&fakeScriptStore{err: errors.New("redis unavailable")}, 5, 90*time.Second, 15*time.Minute)
	if _, err := service.Allow(context.Background(), "tenant-1"); err == nil {
		t.Fatal("Redis error was ignored")
	}
	service = New(&fakeScriptStore{result: []any{int64(1)}}, 5, 90*time.Second, 15*time.Minute)
	if _, err := service.Allow(context.Background(), "tenant-1"); err == nil {
		t.Fatal("malformed script result was accepted")
	}
}
