package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"readergo/internal/media"
	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/gorm"
)

const maxPrepareURLLength = 2048

// PrepareOptions 限制外部下载占用的网络、磁盘与 RabbitMQ 消费时间。
type PrepareOptions struct {
	MaxFileSize           int64
	MaxDuration           time.Duration
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	DownloadTimeout       time.Duration
	MaxRedirects          int
	YTDLPPath             string
	YTDLPMaxHeight        int
}

type prepareRepository interface {
	FindVideo(context.Context, string, string) (model.Video, error)
	SavePreparedVideo(context.Context, string, string, string, int64) error
}

type prepareMediaProber interface {
	Probe(context.Context, string) (media.MediaInfo, error)
}

type prepareObjectStore interface {
	StatObject(context.Context, string) (int64, bool, error)
	DownloadObject(context.Context, string, string) error
	UploadObject(context.Context, string, string, int64) error
}

type prepareSourceDownloader interface {
	Download(context.Context, string, string) (int64, error)
}

// PrepareProcessor 将 URL 源文件安全落入对象存储；任务完成后的下游编排仍由 Consumer 统一处理。
type PrepareProcessor struct {
	repository  prepareRepository
	prober      prepareMediaProber
	objects     prepareObjectStore
	downloader  prepareSourceDownloader
	maxSize     int64
	maxDuration time.Duration
}

// NewPrepareProcessor 创建 URL 下载 Processor，并复用 Worker 已验证的 FFprobe。
func NewPrepareProcessor(db *gorm.DB, ffmpeg *media.FFmpeg, minIOEndpoint, minIOAccessKey, minIOSecretKey string, minIOSecure bool, bucket string, options PrepareOptions) (*PrepareProcessor, error) {
	if db == nil || ffmpeg == nil {
		return nil, fmt.Errorf("prepare processor requires database and ffmpeg")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("prepare processor requires MinIO bucket")
	}
	if err := validatePrepareOptions(options); err != nil {
		return nil, err
	}
	client, err := minio.New(minIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minIOAccessKey, minIOSecretKey, ""),
		Secure: minIOSecure,
	})
	if err != nil {
		return nil, fmt.Errorf("create MinIO client for prepare: %w", err)
	}
	downloader, err := newPrepareDownloader(options, net.DefaultResolver, (&net.Dialer{Timeout: options.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext)
	if err != nil {
		return nil, err
	}
	ytdlp, err := newYTDLPAdapter(options.YTDLPPath, ffmpeg.Path(), downloader, options)
	if err != nil {
		return nil, err
	}
	return &PrepareProcessor{
		repository:  gormPrepareRepository{db: db},
		prober:      ffmpeg,
		objects:     minioPrepareObjectStore{client: client, bucket: bucket},
		downloader:  routingPrepareDownloader{direct: downloader, ytdlp: ytdlp},
		maxSize:     options.MaxFileSize,
		maxDuration: options.MaxDuration,
	}, nil
}

func validatePrepareOptions(options PrepareOptions) error {
	if options.MaxFileSize <= 0 || options.MaxDuration <= 0 || options.ConnectTimeout <= 0 || options.ResponseHeaderTimeout <= 0 || options.DownloadTimeout <= 0 {
		return fmt.Errorf("prepare size and timeout limits must be positive")
	}
	if options.MaxRedirects < 0 || options.MaxRedirects > 10 {
		return fmt.Errorf("prepare max redirects must be between 0 and 10")
	}
	if strings.TrimSpace(options.YTDLPPath) == "" || options.YTDLPMaxHeight < 144 || options.YTDLPMaxHeight > 4320 {
		return fmt.Errorf("prepare yt-dlp path and maximum height are invalid")
	}
	return nil
}

// Process 在稳定 Object Key 上收敛；上传完成但数据库提交失败时，重试会复用已有对象。
func (p *PrepareProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	video, err := p.repository.FindVideo(ctx, job.VideoID, job.TenantID)
	if err != nil {
		return fmt.Errorf("load prepare video %s: %w", job.VideoID, err)
	}
	objectKey := fmt.Sprintf("tenants/%s/videos/%s/source/original", job.TenantID, job.VideoID)
	size, exists, err := p.objects.StatObject(ctx, objectKey)
	if err != nil {
		return fmt.Errorf("stat prepared object %q: %w", objectKey, err)
	}
	if exists && (size <= 0 || size > p.maxSize) {
		return permanentProcessError(fmt.Errorf("prepared object %q has invalid size %d", objectKey, size))
	}
	if exists && video.ObjectKey == objectKey && video.DurationMS > 0 {
		// 数据库和最终对象均已提交时直接成功，重复消息不会再次下载、写对象或创建下游任务。
		return nil
	}

	workDir, err := os.MkdirTemp("", "readergo-prepare-*")
	if err != nil {
		return fmt.Errorf("create prepare work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	videoPath := filepath.Join(workDir, "source-video")

	if exists {
		// 上一次可能在对象上传后、数据库提交前退出；只从 MinIO 恢复校验，不重复访问外部来源。
		if err := p.objects.DownloadObject(ctx, objectKey, videoPath); err != nil {
			return fmt.Errorf("restore prepared object %q: %w", objectKey, err)
		}
	} else {
		downloadedSize, err := p.downloader.Download(ctx, video.SourceURL, videoPath)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isPrepareInputError(err) {
				return userInputProcessError(err)
			}
			return fmt.Errorf("download source video: %w", err)
		}
		if downloadedSize <= 0 {
			return userInputProcessError(fmt.Errorf("source video is empty"))
		}
	}

	info, err := p.prober.Probe(ctx, videoPath)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if exists {
			return permanentProcessError(fmt.Errorf("validate existing prepared object: %w", err))
		}
		return userInputProcessError(fmt.Errorf("source URL did not return a valid video: %w", err))
	}
	if err := validatePreparedMedia(info, p.maxDuration); err != nil {
		if exists {
			return permanentProcessError(err)
		}
		return userInputProcessError(err)
	}

	if !exists {
		fileInfo, err := os.Stat(videoPath)
		if err != nil {
			return fmt.Errorf("stat downloaded video: %w", err)
		}
		if err := p.objects.UploadObject(ctx, objectKey, videoPath, fileInfo.Size()); err != nil {
			return fmt.Errorf("upload prepared object %q: %w", objectKey, err)
		}
	}
	if err := p.repository.SavePreparedVideo(ctx, video.ID, video.TenantID, objectKey, info.DurationMS); err != nil {
		return fmt.Errorf("store prepared video metadata: %w", err)
	}
	return nil
}

func validatePreparedMedia(info media.MediaInfo, maxDuration time.Duration) error {
	if info.DurationMS <= 0 {
		return fmt.Errorf("source video has invalid duration %d ms", info.DurationMS)
	}
	if strings.TrimSpace(info.FormatName) == "" || strings.TrimSpace(info.VideoCodec) == "" || info.Width <= 0 || info.Height <= 0 {
		return fmt.Errorf("source URL did not return media with a valid video stream")
	}
	if info.DurationMS > maxDuration.Milliseconds() {
		return fmt.Errorf("source video duration %d ms exceeds maximum %s", info.DurationMS, maxDuration)
	}
	return nil
}

type gormPrepareRepository struct{ db *gorm.DB }

func (r gormPrepareRepository) FindVideo(ctx context.Context, videoID, tenantID string) (model.Video, error) {
	var video model.Video
	err := r.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", videoID, tenantID).First(&video).Error
	return video, err
}

func (r gormPrepareRepository) SavePreparedVideo(ctx context.Context, videoID, tenantID, objectKey string, durationMS int64) error {
	result := r.db.WithContext(ctx).Model(&model.Video{}).
		Where("id = ? AND tenant_id = ?", videoID, tenantID).
		Updates(map[string]any{"object_key": objectKey, "duration_ms": durationMS})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("video %s no longer belongs to tenant %s", videoID, tenantID)
	}
	return nil
}

type minioPrepareObjectStore struct {
	client *minio.Client
	bucket string
}

func (s minioPrepareObjectStore) StatObject(ctx context.Context, objectKey string) (int64, bool, error) {
	info, err := s.client.StatObject(ctx, s.bucket, objectKey, minio.StatObjectOptions{})
	if err == nil {
		return info.Size, true, nil
	}
	response := minio.ToErrorResponse(err)
	if response.Code == "NoSuchKey" || response.Code == "NoSuchObject" || response.Code == "NotFound" {
		return 0, false, nil
	}
	return 0, false, err
}

func (s minioPrepareObjectStore) DownloadObject(ctx context.Context, objectKey, destination string) error {
	return s.client.FGetObject(ctx, s.bucket, objectKey, destination, minio.GetObjectOptions{})
}

func (s minioPrepareObjectStore) UploadObject(ctx context.Context, objectKey, source string, size int64) error {
	info, err := s.client.FPutObject(ctx, s.bucket, objectKey, source, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return err
	}
	if info.Size != size {
		return fmt.Errorf("MinIO stored %d bytes, expected %d", info.Size, size)
	}
	return nil
}

type prepareResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type prepareDialContext func(context.Context, string, string) (net.Conn, error)

type prepareInputError struct{ err error }

func (e *prepareInputError) Error() string { return e.err.Error() }
func (e *prepareInputError) Unwrap() error { return e.err }

func prepareInput(err error) error {
	if err == nil {
		return nil
	}
	return &prepareInputError{err: err}
}

func isPrepareInputError(err error) bool {
	var inputErr *prepareInputError
	return errors.As(err, &inputErr)
}

type prepareDownloader struct {
	client   *http.Client
	resolver prepareResolver
	options  PrepareOptions
}

func newPrepareDownloader(options PrepareOptions, resolver prepareResolver, dial prepareDialContext) (*prepareDownloader, error) {
	if err := validatePrepareOptions(options); err != nil {
		return nil, err
	}
	if resolver == nil || dial == nil {
		return nil, fmt.Errorf("prepare downloader requires resolver and dialer")
	}
	d := &prepareDownloader{resolver: resolver, options: options}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.dialValidatedTarget(dial),
		DisableCompression:    true,
		ResponseHeaderTimeout: options.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   options.ConnectTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	d.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > options.MaxRedirects {
				return prepareInput(fmt.Errorf("source URL exceeded %d redirects", options.MaxRedirects))
			}
			target, err := parsePrepareURL(request.URL.String())
			if err != nil {
				return err
			}
			// 每次跳转都重新解析 DNS；DialContext 连接前还会再次校验，阻断 DNS rebinding。
			return d.validateTarget(request.Context(), target)
		},
	}
	return d, nil
}

func (d *prepareDownloader) Download(ctx context.Context, rawURL, destination string) (int64, error) {
	target, err := parsePrepareURL(rawURL)
	if err != nil {
		return 0, err
	}
	if err := d.validateTarget(ctx, target); err != nil {
		return 0, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, d.options.DownloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, prepareInput(fmt.Errorf("create source request: %w", err))
	}
	request.Header.Set("Accept", "video/*,application/octet-stream;q=0.9,*/*;q=0.1")
	request.Header.Set("User-Agent", "ReaderGo/1.0 video-prepare")
	response, err := d.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("source server returned HTTP %d", response.StatusCode)
		if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusTooEarly {
			return 0, prepareInput(err)
		}
		return 0, err
	}
	if response.ContentLength > d.options.MaxFileSize {
		return 0, prepareInput(fmt.Errorf("source video exceeds maximum size of %d bytes", d.options.MaxFileSize))
	}

	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create prepare temporary file: %w", err)
	}
	// LimitReader 多读取 1 字节来识别缺失或伪造 Content-Length 的超限响应。
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, d.options.MaxFileSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		return written, fmt.Errorf("stream source video: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("close prepare temporary file: %w", closeErr)
	}
	if written > d.options.MaxFileSize {
		return written, prepareInput(fmt.Errorf("source video exceeds maximum size of %d bytes", d.options.MaxFileSize))
	}
	return written, nil
}

func parsePrepareURL(rawURL string) (*url.URL, error) {
	if len(rawURL) == 0 || len(rawURL) > maxPrepareURLLength {
		return nil, prepareInput(fmt.Errorf("source URL length must be between 1 and %d bytes", maxPrepareURLLength))
	}
	target, err := url.ParseRequestURI(rawURL)
	if err != nil || target.Host == "" || target.Hostname() == "" {
		return nil, prepareInput(fmt.Errorf("source URL must be an absolute HTTP(S) URL"))
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, prepareInput(fmt.Errorf("source URL scheme %q is not allowed", target.Scheme))
	}
	if target.User != nil {
		return nil, prepareInput(fmt.Errorf("source URL must not contain username or password"))
	}
	if port := target.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, prepareInput(fmt.Errorf("source URL contains an invalid port"))
		}
	}
	return target, nil
}

func (d *prepareDownloader) validateTarget(ctx context.Context, target *url.URL) error {
	addresses, err := d.resolveAllowedIPs(ctx, target.Hostname())
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return fmt.Errorf("source host %q resolved to no addresses", target.Hostname())
	}
	return nil
}

func (d *prepareDownloader) resolveAllowedIPs(ctx context.Context, hostname string) ([]net.IP, error) {
	if literal := net.ParseIP(hostname); literal != nil {
		if isRestrictedPrepareIP(literal) {
			return nil, prepareInput(fmt.Errorf("source host resolves to a non-public address"))
		}
		return []net.IP{literal}, nil
	}
	addresses, err := d.resolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("resolve source host %q: %w", hostname, err)
	}
	allowed := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if isRestrictedPrepareIP(address.IP) {
			// 混合返回公网和内网地址同样拒绝，避免解析顺序变化时偶发访问内网。
			return nil, prepareInput(fmt.Errorf("source host resolves to a non-public address"))
		}
		allowed = append(allowed, address.IP)
	}
	return allowed, nil
}

func isRestrictedPrepareIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() || isCarrierGradeNAT(ip)
}

func isCarrierGradeNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40
}

func (d *prepareDownloader) dialValidatedTarget(dial prepareDialContext) prepareDialContext {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, prepareInput(fmt.Errorf("invalid source address %q", address))
		}
		addresses, err := d.resolveAllowedIPs(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range addresses {
			connection, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("source host resolved to no addresses")
		}
		return nil, lastErr
	}
}
