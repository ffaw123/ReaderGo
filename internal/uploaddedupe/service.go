package uploaddedupe

import (
	"context"
	"errors"
	"log"
	"time"

	"readergo/internal/cache"
	"readergo/internal/model"

	"gorm.io/gorm"
)

const keyVersion = "v1"

type Entry struct {
	VideoID string `json:"video_id"`
}

type videoRepository interface {
	FindActiveBySHA256(ctx context.Context, tenantID, sha256 string) (string, bool, error)
}

type gormVideoRepository struct {
	db *gorm.DB
}

func (r gormVideoRepository) FindActiveBySHA256(ctx context.Context, tenantID, sha256 string) (string, bool, error) {
	var video model.Video
	err := r.db.WithContext(ctx).Select("id").
		Where("tenant_id = ? AND content_sha256 = ? AND status <> ?", tenantID, sha256, model.VideoStatusFailed).
		Take(&video).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return video.ID, true, nil
}

// Service 将上传去重规则与通用 Redis 客户端隔离，PostgreSQL 始终是最终事实来源。
type Service struct {
	repository videoRepository
	cache      cache.Store
	ttl        time.Duration
}

func New(db *gorm.DB, cacheStore cache.Store, ttl time.Duration) *Service {
	return &Service{repository: gormVideoRepository{db: db}, cache: cacheStore, ttl: ttl}
}

func (s *Service) key(tenantID, sha256 string) string {
	return s.cache.Key("upload-dedup", keyVersion, tenantID, sha256)
}

// Find 先查 Redis；缓存故障或未命中时回源数据库，并在数据库命中后回填缓存。
func (s *Service) Find(ctx context.Context, tenantID, sha256 string) (Entry, bool, error) {
	key := s.key(tenantID, sha256)
	var entry Entry
	if found, err := s.cache.GetJSON(ctx, key, &entry); err != nil {
		log.Printf("read upload dedup cache: %v", err)
	} else if found && entry.VideoID != "" {
		return entry, true, nil
	}

	videoID, found, err := s.repository.FindActiveBySHA256(ctx, tenantID, sha256)
	if err != nil || !found {
		return Entry{}, found, err
	}
	entry = Entry{VideoID: videoID}
	s.Remember(ctx, tenantID, sha256, videoID)
	return entry, true, nil
}

// Remember 在数据库状态提交后写入正缓存；缓存失败不能反向破坏已完成的数据库事务。
func (s *Service) Remember(ctx context.Context, tenantID, sha256, videoID string) {
	if sha256 == "" || videoID == "" {
		return
	}
	if err := s.cache.SetJSON(ctx, s.key(tenantID, sha256), Entry{VideoID: videoID}, s.ttl); err != nil {
		log.Printf("write upload dedup cache: %v", err)
	}
}

// Forget 在视频退出有效去重集合后删除缓存；失败时由固定 TTL 最终自愈。
func (s *Service) Forget(ctx context.Context, tenantID, sha256 string) {
	if sha256 == "" {
		return
	}
	if err := s.cache.Delete(ctx, s.key(tenantID, sha256)); err != nil {
		log.Printf("delete upload dedup cache: %v", err)
	}
}
