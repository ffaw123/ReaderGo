package visionagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"readergo/internal/model"
)

type fakeVideoSource struct {
	calls int
}

func (s *fakeVideoSource) LocalPath(_ context.Context, _ model.Video) (string, func(), error) {
	s.calls++
	directory, err := os.MkdirTemp("", "readergo-vision-cache-test-*")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(directory, "video")
	if err := os.WriteFile(path, []byte("video-content"), 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(directory) }, nil
}

func TestCachedVideoSourceReusesDownloadedVideo(t *testing.T) {
	upstream := &fakeVideoSource{}
	cache, err := NewCachedVideoSource(upstream, 2, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	video := model.Video{ID: "video-1", ObjectKey: "videos/video-1/source", UpdatedAt: time.Unix(1, 0)}
	first, releaseFirst, err := cache.LocalPath(context.Background(), video)
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst()
	second, releaseSecond, err := cache.LocalPath(context.Background(), video)
	if err != nil {
		t.Fatal(err)
	}
	releaseSecond()
	if first != second || upstream.calls != 1 {
		t.Fatalf("cache paths/calls = %q/%q/%d, want same path and one call", first, second, upstream.calls)
	}
}

func TestCachedVideoSourceRejectsFileLargerThanDiskBudget(t *testing.T) {
	upstream := &fakeVideoSource{}
	cache, err := NewCachedVideoSource(upstream, 2, 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	video := model.Video{ID: "video-too-large", ObjectKey: "videos/large/source", UpdatedAt: time.Unix(1, 0)}
	if _, _, err := cache.LocalPath(context.Background(), video); err == nil {
		t.Fatal("expected cache disk budget error")
	}
	entries, err := os.ReadDir(cache.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache left %d files after rejecting oversized source", len(entries))
	}
}
