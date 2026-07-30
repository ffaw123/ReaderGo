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
	return s.client.NewMultipartUpload(ctx, s.bucket, objectKey, minio.PutObjectOptions{ContentType: contentType})
}

// Abort 在 API 无法创建可用上传会话时清理未完成分片。
func (s *MultipartStore) Abort(ctx context.Context, objectKey, uploadID string) error {
	return s.client.AbortMultipartUpload(ctx, s.bucket, objectKey, uploadID)
}

// PresignedPartURL 仅允许对指定对象、upload ID 和 part number 发起一次 PUT。
func (s *MultipartStore) PresignedPartURL(ctx context.Context, objectKey, uploadID string, partNumber int) (string, time.Time, error) {
	// S3 使用这两个查询参数定位 Multipart Upload 及其指定分片。
	params := url.Values{"partNumber": {""}, "uploadId": {uploadID}}
	params.Set("partNumber", fmt.Sprintf("%d", partNumber))
	// 必须签名 PUT 而不是通用对象 URL，防止该地址被用于下载或删除数据。
	u, err := s.client.Presign(ctx, http.MethodPut, s.bucket, objectKey, s.urlTTL, params)
	if err != nil {
		return "", time.Time{}, err
	}
	return u.String(), time.Now().Add(s.urlTTL), nil
}
