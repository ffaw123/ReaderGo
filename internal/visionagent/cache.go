package visionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"readergo/internal/model"
)

type cachedSourceEntry struct {
	path     string
	size     int64
	lastUsed time.Time
	refs     int
}

type cachedSourceLoad struct {
	done chan struct{}
	err  error
}

// CachedVideoSource 在受控临时目录缓存少量原视频，重叠请求不会重复从 MinIO 下载。
type CachedVideoSource struct {
	upstream   VideoSource
	root       string
	maxEntries int
	maxBytes   int64
	usedBytes  int64
	ttl        time.Duration
	mu         sync.Mutex
	entries    map[string]*cachedSourceEntry
	loads      map[string]*cachedSourceLoad
}

func NewCachedVideoSource(upstream VideoSource, maxEntries int, maxBytes int64, ttl time.Duration) (*CachedVideoSource, error) {
	if upstream == nil || maxEntries < 1 || maxBytes < 1 || ttl <= 0 {
		return nil, fmt.Errorf("vision source cache options are invalid")
	}
	root, err := os.MkdirTemp("", "readergo-vision-cache-*")
	if err != nil {
		return nil, fmt.Errorf("create vision cache directory: %w", err)
	}
	return &CachedVideoSource{
		upstream: upstream, root: root, maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl,
		entries: make(map[string]*cachedSourceEntry), loads: make(map[string]*cachedSourceLoad),
	}, nil
}

// Close 清理本进程创建的缓存根目录；应只在停止接收请求后调用。
func (s *CachedVideoSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.RemoveAll(s.root)
	s.entries = make(map[string]*cachedSourceEntry)
	s.usedBytes = 0
	return err
}

func (s *CachedVideoSource) LocalPath(ctx context.Context, video model.Video) (string, func(), error) {
	key := visionVideoCacheKey(video)
	for {
		s.mu.Lock()
		s.evictExpiredLocked(time.Now())
		if entry := s.entries[key]; entry != nil {
			entry.refs++
			entry.lastUsed = time.Now()
			path := entry.path
			s.mu.Unlock()
			return path, s.releaseFunc(key), nil
		}
		if load := s.loads[key]; load != nil {
			done := load.done
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-done:
				if load.err != nil {
					return "", nil, load.err
				}
				continue
			}
		}
		load := &cachedSourceLoad{done: make(chan struct{})}
		s.loads[key] = load
		s.mu.Unlock()

		path, size, err := s.populate(ctx, video, key)
		s.mu.Lock()
		if err == nil && !s.makeRoomLocked(size) {
			err = fmt.Errorf("vision source cache capacity is temporarily unavailable")
			_ = os.Remove(path)
		}
		load.err = err
		delete(s.loads, key)
		if err == nil {
			s.entries[key] = &cachedSourceEntry{path: path, size: size, lastUsed: time.Now(), refs: 1}
			s.usedBytes += size
		}
		close(load.done)
		s.mu.Unlock()
		if err != nil {
			return "", nil, err
		}
		return path, s.releaseFunc(key), nil
	}
}

func (s *CachedVideoSource) populate(ctx context.Context, video model.Video, key string) (string, int64, error) {
	sourcePath, release, err := s.upstream.LocalPath(ctx, video)
	if err != nil {
		return "", 0, err
	}
	defer release()
	temporary, err := os.CreateTemp(s.root, key+"-*.partial")
	if err != nil {
		return "", 0, fmt.Errorf("create vision cache file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	source, err := os.Open(sourcePath)
	if err != nil {
		_ = temporary.Close()
		return "", 0, fmt.Errorf("open downloaded vision source: %w", err)
	}
	size, copyErr := io.Copy(temporary, source)
	closeSourceErr := source.Close()
	closeTargetErr := temporary.Close()
	if copyErr != nil || closeSourceErr != nil || closeTargetErr != nil {
		return "", 0, fmt.Errorf("copy vision source into cache: %w", firstError(copyErr, closeSourceErr, closeTargetErr))
	}
	finalPath := filepath.Join(s.root, key+".video")
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", 0, fmt.Errorf("commit vision cache file: %w", err)
	}
	return finalPath, size, nil
}

func (s *CachedVideoSource) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if entry := s.entries[key]; entry != nil && entry.refs > 0 {
				entry.refs--
				entry.lastUsed = time.Now()
			}
		})
	}
}

func (s *CachedVideoSource) evictExpiredLocked(now time.Time) {
	for key, entry := range s.entries {
		if entry.refs == 0 && now.Sub(entry.lastUsed) >= s.ttl {
			_ = os.Remove(entry.path)
			s.usedBytes -= entry.size
			delete(s.entries, key)
		}
	}
}

func (s *CachedVideoSource) makeRoomLocked(incomingBytes int64) bool {
	if incomingBytes > s.maxBytes {
		return false
	}
	for len(s.entries) >= s.maxEntries || s.usedBytes+incomingBytes > s.maxBytes {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.entries {
			if entry.refs == 0 && (oldestKey == "" || entry.lastUsed.Before(oldest)) {
				oldestKey, oldest = key, entry.lastUsed
			}
		}
		if oldestKey == "" {
			return false
		}
		_ = os.Remove(s.entries[oldestKey].path)
		s.usedBytes -= s.entries[oldestKey].size
		delete(s.entries, oldestKey)
	}
	return true
}

func visionVideoCacheKey(video model.Video) string {
	identity := video.ID + "\x00" + video.ObjectKey + "\x00" + video.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if video.ContentSHA256 != nil {
		identity += "\x00" + *video.ContentSHA256
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
