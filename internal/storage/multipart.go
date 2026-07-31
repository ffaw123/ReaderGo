package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MultipartStore struct {
	// client 使用 MinIO 的低层 Core API，以调用 S3 Multipart Upload 原语。
	client *minio.Core
	// bucket 是所有本地上传原始视频写入的受控存储桶。
	bucket string
	// urlTTL 限制浏览器直传授权窗口，避免使用长效 URL。
	urlTTL time.Duration
}

const (
	// maxRetries 不包含首次请求；失败后最多额外重试 3 次。
	maxRetries = 3
	retryDelay = 100 * time.Millisecond
)

// CompletedPart 是浏览器直传成功后返回的不可伪造分片 ETag。
// 只有服务端使用保存的 upload ID 调用 Complete，浏览器无法自行拼接对象。
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// NewMultipartStore 只创建客户端；EnsureBucket 在启动阶段执行存储桶可用性检查。
func NewMultipartStore(endpoint, accessKey, secretKey, bucket string, secure bool, urlTTL time.Duration) (*MultipartStore, error) {
	// Access Key 和 Secret Key 仅保留在 API 服务端；浏览器只能获得预签名 URL。
	client, err := minio.NewCore(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, err
	}
	return &MultipartStore{client: client, bucket: bucket, urlTTL: urlTTL}, nil
}

// EnsureBucket 仅在目标 bucket 不存在时创建它。
func (s *MultipartStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil || exists {
		return err
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

// Create 启动一次 multipart upload，并返回仅由服务端保存的 upload ID。
func (s *MultipartStore) Create(ctx context.Context, objectKey, contentType string) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		uploadID, err := s.client.NewMultipartUpload(ctx, s.bucket, objectKey, minio.PutObjectOptions{ContentType: contentType})
		if err == nil {
			return uploadID, nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}

		delay := time.Duration(attempt+1) * retryDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

// Abort 在 API 无法创建可用上传会话时清理未完成分片，同时uploadID直接失效。
func (s *MultipartStore) Abort(ctx context.Context, objectKey, uploadID string) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := s.client.AbortMultipartUpload(ctx, s.bucket, objectKey, uploadID)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}

		delay := time.Duration(attempt+1) * retryDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// Complete 校验通过后的全部分片写入一个最终对象，并关闭 multipart upload。
func (s *MultipartStore) Complete(ctx context.Context, objectKey, uploadID string, parts []CompletedPart) error {
	completed := make([]minio.CompletePart, len(parts))
	for i, part := range parts {
		completed[i] = minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		_, err := s.client.CompleteMultipartUpload(ctx, s.bucket, objectKey, uploadID, completed, minio.PutObjectOptions{})
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}

		delay := time.Duration(attempt+1) * retryDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// PresignedPartURL 仅允许对指定对象、upload ID 和 part number 发起一次 PUT。
func (s *MultipartStore) PresignedPartURL(ctx context.Context, objectKey, uploadID string, partNumber int) (string, time.Time, error) {
	// S3 使用这两个查询参数定位 Multipart Upload 及其指定分片。
	params := url.Values{"partNumber": {""}, "uploadId": {uploadID}}
	params.Set("partNumber", fmt.Sprintf("%d", partNumber))

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 必须签名 PUT 而不是通用对象 URL，防止该地址被用于下载或删除数据。
		u, err := s.client.Presign(ctx, http.MethodPut, s.bucket, objectKey, s.urlTTL, params)
		if err == nil {
			return u.String(), time.Now().Add(s.urlTTL), nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}

		// 退避等待必须响应请求取消，不能让客户端取消后仍阻塞在重试中。
		delay := time.Duration(attempt+1) * retryDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", time.Time{}, ctx.Err()
		case <-timer.C:
		}
	}
	return "", time.Time{}, lastErr
}
