package uploaddedupe

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRepository struct {
	videoID string
	found   bool
	err     error
	calls   int
}

func (r *fakeRepository) FindActiveBySHA256(context.Context, string, string) (string, bool, error) {
	r.calls++
	return r.videoID, r.found, r.err
}

type fakeCache struct {
	values    map[string][]byte
	getErr    error
	setErr    error
	deleteErr error
	setTTL    time.Duration
}

func (c *fakeCache) Key(parts ...string) string { return "test:" + strings.Join(parts, ":") }
func (c *fakeCache) GetJSON(_ context.Context, key string, dst any) (bool, error) {
	if c.getErr != nil {
		return false, c.getErr
	}
	value, found := c.values[key]
	if !found {
		return false, nil
	}
	return true, json.Unmarshal(value, dst)
}
func (c *fakeCache) SetJSON(_ context.Context, key string, value any, ttl time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		c.values[key], c.setTTL = encoded, ttl
	}
	return err
}
func (c *fakeCache) Delete(_ context.Context, keys ...string) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	for _, key := range keys {
		delete(c.values, key)
	}
	return nil
}
func (*fakeCache) SetNX(context.Context, string, any, time.Duration) (bool, error) { return false, nil }

func TestFindUsesCacheBeforeDatabase(t *testing.T) {
	cacheStore := &fakeCache{values: map[string][]byte{}}
	repository := &fakeRepository{err: errors.New("database must not be queried")}
	service := &Service{repository: repository, cache: cacheStore, ttl: 15 * time.Minute}
	key := service.key("tenant-1", "sha-1")
	cacheStore.values[key], _ = json.Marshal(Entry{VideoID: "video-cache"})

	entry, found, err := service.Find(context.Background(), "tenant-1", "sha-1")
	if err != nil || !found || entry.VideoID != "video-cache" || repository.calls != 0 {
		t.Fatalf("Find = (%+v, %v, %v), repository calls = %d", entry, found, err, repository.calls)
	}
}

func TestFindFallsBackAndWarmsCache(t *testing.T) {
	cacheStore := &fakeCache{values: map[string][]byte{}, getErr: errors.New("redis unavailable")}
	repository := &fakeRepository{videoID: "video-db", found: true}
	service := &Service{repository: repository, cache: cacheStore, ttl: 15 * time.Minute}

	entry, found, err := service.Find(context.Background(), "tenant-1", "sha-1")
	if err != nil || !found || entry.VideoID != "video-db" || repository.calls != 1 {
		t.Fatalf("Find = (%+v, %v, %v), repository calls = %d", entry, found, err, repository.calls)
	}
	if cacheStore.setTTL != 15*time.Minute {
		t.Fatalf("cache ttl = %v, want 15m", cacheStore.setTTL)
	}
}

func TestForgetUsesTenantScopedKey(t *testing.T) {
	cacheStore := &fakeCache{values: map[string][]byte{}}
	service := &Service{repository: &fakeRepository{}, cache: cacheStore, ttl: 15 * time.Minute}
	key := service.key("tenant-1", "sha-1")
	cacheStore.values[key] = []byte(`{"video_id":"video-1"}`)

	service.Forget(context.Background(), "tenant-1", "sha-1")
	if _, found := cacheStore.values[key]; found {
		t.Fatal("dedup cache key was not deleted")
	}
}
