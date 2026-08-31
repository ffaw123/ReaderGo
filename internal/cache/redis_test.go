package cache

import (
	"context"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
)

func TestRedisJSONAndTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client, err := NewRedis(context.Background(), "redis://"+server.Addr()+"/0", "readergo-test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	key := client.Key("upload-dedup", "v1", "tenant", "sha")
	if key != "readergo-test:upload-dedup:v1:tenant:sha" {
		t.Fatalf("key = %q", key)
	}
	type entry struct {
		VideoID string `json:"video_id"`
	}
	if err := client.SetJSON(context.Background(), key, entry{VideoID: "video-1"}, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := server.TTL(key); ttl != 15*time.Minute {
		t.Fatalf("ttl = %v, want 15m", ttl)
	}
	var got entry
	found, err := client.GetJSON(context.Background(), key, &got)
	if err != nil || !found || got.VideoID != "video-1" {
		t.Fatalf("GetJSON = (%+v, %v, %v)", got, found, err)
	}
	if err := client.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if found, err := client.GetJSON(context.Background(), key, &got); err != nil || found {
		t.Fatalf("deleted GetJSON = (%v, %v), want miss", found, err)
	}
}

func TestRedisSetNX(t *testing.T) {
	server := miniredis.RunT(t)
	client, err := NewRedis(context.Background(), "redis://"+server.Addr()+"/0", "readergo-test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	key := client.Key("lock", "one")
	created, err := client.SetNX(context.Background(), key, map[string]string{"token": "first"}, time.Minute)
	if err != nil || !created {
		t.Fatalf("first SetNX = (%v, %v)", created, err)
	}
	created, err = client.SetNX(context.Background(), key, map[string]string{"token": "second"}, time.Minute)
	if err != nil || created {
		t.Fatalf("second SetNX = (%v, %v), want false", created, err)
	}
}

func TestRedisEval(t *testing.T) {
	server := miniredis.RunT(t)
	client, err := NewRedis(context.Background(), "redis://"+server.Addr()+"/0", "readergo-test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	key := client.Key("script", "counter")
	result, err := client.Eval(context.Background(), `return redis.call('INCR', KEYS[1])`, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.(int64); !ok || value != 1 {
		t.Fatalf("Eval result = %#v, want int64(1)", result)
	}
}
