package visionagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"readergo/internal/model"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type VideoSource interface {
	LocalPath(context.Context, model.Video) (string, func(), error)
}

// MinIOVideoSource 为一次在线画面请求创建受控临时副本，请求结束后立即清理。
type MinIOVideoSource struct {
	client *minio.Client
	bucket string
}

func NewMinIOVideoSource(endpoint, accessKey, secretKey string, secure bool, bucket string) (*MinIOVideoSource, error) {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("vision MinIO endpoint and bucket are required")
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("create vision MinIO client: %w", err)
	}
	return &MinIOVideoSource{client: client, bucket: bucket}, nil
}

func (s *MinIOVideoSource) LocalPath(ctx context.Context, video model.Video) (string, func(), error) {
	workDir, err := os.MkdirTemp("", "readergo-vision-source-*")
	if err != nil {
		return "", nil, fmt.Errorf("create vision source directory: %w", err)
	}
	release := func() { _ = os.RemoveAll(workDir) }
	path := filepath.Join(workDir, "input-video")
	if err := s.client.FGetObject(ctx, s.bucket, video.ObjectKey, path, minio.GetObjectOptions{}); err != nil {
		release()
		return "", nil, fmt.Errorf("download vision video source: %w", err)
	}
	return path, release, nil
}
