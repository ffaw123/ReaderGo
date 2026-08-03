package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log"
	"math/bits"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"readergo/internal/media"
	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/gorm"
)

const (
	ocrFixedSampleInterval = 15 * time.Second
	ocrMaxAttempts         = 3
	ocrRequestTimeout      = 60 * time.Second
)

var ocrRetryDelays = [...]time.Duration{500 * time.Millisecond, time.Second}

type ocrFrame struct {
	TimestampMS int64
	Path        string
	Hash        uint64
}

type ocrRecognition struct {
	Text       string
	Confidence float64
}

type completedOCRRecognition struct {
	Result ocrRecognition
	Done   bool
}

type ocrServiceUnavailableError struct{ cause error }

func (e *ocrServiceUnavailableError) Error() string {
	return fmt.Sprintf("OCR service unavailable after %d attempts: %v", ocrMaxAttempts, e.cause)
}

func (e *ocrServiceUnavailableError) Unwrap() error { return e.cause }

// OCRProcessor 实现现有 vision 阶段：从视频抽帧后调用 PaddleOCR，并保存原始视觉文字。
type OCRProcessor struct {
	db             *gorm.DB
	ffmpeg         *media.FFmpeg
	objects        *minio.Client
	bucket         string
	endpoint       string
	client         *http.Client
	minConfidence  float64
	sceneThreshold float64
	hashDistance   int
	workers        int
	retryDelays    []time.Duration
}

// NewOCRProcessor 只校验配置并创建可复用客户端；OCR 服务临时不可用不会阻止 Worker 启动。
func NewOCRProcessor(db *gorm.DB, ffmpeg *media.FFmpeg, minIOEndpoint, minIOAccessKey, minIOSecretKey string, minIOSecure bool, bucket, baseURL string, minConfidence, sceneThreshold float64, hashDistance, concurrency int) (*OCRProcessor, error) {
	if db == nil || ffmpeg == nil {
		return nil, fmt.Errorf("OCR processor requires database and ffmpeg")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("OCR processor requires MinIO bucket")
	}
	endpointURL, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || (endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Host == "" {
		return nil, fmt.Errorf("OCR_BASE_URL must be an absolute HTTP(S) URL")
	}
	if minConfidence < 0 || minConfidence > 1 {
		return nil, fmt.Errorf("OCR minimum confidence must be between 0 and 1")
	}
	if sceneThreshold <= 0 || sceneThreshold > 1 {
		return nil, fmt.Errorf("OCR scene threshold must be greater than 0 and at most 1")
	}
	if hashDistance < 0 || hashDistance > 64 {
		return nil, fmt.Errorf("OCR hash distance must be between 0 and 64")
	}
	if concurrency < 1 || concurrency > 4 {
		return nil, fmt.Errorf("OCR concurrency must be between 1 and 4")
	}

	objects, err := minio.New(minIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minIOAccessKey, minIOSecretKey, ""),
		Secure: minIOSecure,
	})
	if err != nil {
		return nil, fmt.Errorf("create MinIO client for OCR: %w", err)
	}
	return &OCRProcessor{
		db: db, ffmpeg: ffmpeg, objects: objects, bucket: bucket,
		endpoint: endpointURL.String(), client: &http.Client{Timeout: ocrRequestTimeout},
		minConfidence: minConfidence, sceneThreshold: sceneThreshold, hashDistance: hashDistance, workers: concurrency,
		retryDelays: ocrRetryDelays[:],
	}, nil
}

func (p *OCRProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	return p.consumeOCR(ctx, job)
}

func (p *OCRProcessor) consumeOCR(ctx context.Context, job queue.VideoJobMessage) error {
	var video model.Video
	if err := p.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", job.VideoID, job.TenantID).First(&video).Error; err != nil {
		return fmt.Errorf("load OCR video %s: %w", job.VideoID, err)
	}
	if strings.TrimSpace(video.ObjectKey) == "" {
		return fmt.Errorf("OCR video %s has no object key", video.ID)
	}

	workDir, err := os.MkdirTemp("", "readergo-ocr-*")
	if err != nil {
		return fmt.Errorf("create OCR work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	videoPath := filepath.Join(workDir, "input-video")
	if err := p.objects.FGetObject(ctx, p.bucket, video.ObjectKey, videoPath, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download video object %q for OCR: %w", video.ObjectKey, err)
	}
	info, err := p.ffmpeg.Probe(ctx, videoPath)
	if err != nil {
		return err
	}
	if info.DurationMS <= 0 {
		return fmt.Errorf("video %s has invalid duration %d ms", video.ID, info.DurationMS)
	}
	if info.VideoCodec == "" {
		return fmt.Errorf("video %s does not contain a video stream", video.ID)
	}
	if err := p.db.WithContext(ctx).Model(&model.Video{}).Where("id = ?", video.ID).Update("duration_ms", info.DurationMS).Error; err != nil {
		return fmt.Errorf("store video duration: %w", err)
	}

	// 固定采样和场景采样共享一次视频解码，避免逐帧启动 FFmpeg 并重复解码前序内容。
	samples, err := p.ffmpeg.ExtractOCRFrames(ctx, videoPath, workDir, time.Duration(info.DurationMS)*time.Millisecond, ocrFixedSampleInterval, p.sceneThreshold)
	if err != nil {
		return err
	}
	frames, err := p.deduplicateFrames(ctx, samples)
	if err != nil {
		return err
	}

	segments := make([]model.OCRSegment, 0, len(frames))
	results, recognizeErr := p.recognizeFrames(ctx, frames)
	completedCount := len(frames)
	if recognizeErr != nil {
		if unavailable, ok := recognizeErr.(*ocrServiceUnavailableError); ok {
			// 并发请求中任一请求确认服务不可用后，仅保留连续完成的前缀，剩余时间轴统一标记降级。
			completedCount = 0
			for completedCount < len(results) && results[completedCount].Done {
				completedCount++
			}
			log.Printf("OCR service unavailable for video %s from frame %d: %v", video.ID, completedCount, unavailable)
		} else {
			return recognizeErr
		}
	}
	for index := 0; index < completedCount; index++ {
		frame := frames[index]
		endMS := info.DurationMS
		if index+1 < len(frames) {
			endMS = frames[index+1].TimestampMS
		}
		if endMS <= frame.TimestampMS {
			// 时间轴异常时不写入非法 [t,t) 区间，数据库约束作为最后一道保护仍然保留。
			continue
		}
		result := results[index].Result
		status := model.OCRStatusSucceeded
		if result.Text == "" {
			status = model.OCRStatusNoText
		}
		segments = append(segments, model.OCRSegment{TenantID: video.TenantID, VideoID: video.ID, StartMS: frame.TimestampMS, EndMS: endMS, Text: result.Text, Confidence: result.Confidence, Status: status})
	}
	if completedCount < len(frames) {
		startMS := frames[completedCount].TimestampMS
		if startMS < info.DurationMS {
			segments = append(segments, model.OCRSegment{TenantID: video.TenantID, VideoID: video.ID, StartMS: startMS, EndMS: info.DurationMS, Text: "", Confidence: 0, Status: model.OCRStatusServiceUnavailable})
		}
	}
	if err := p.replaceOCRSegments(ctx, video.ID, segments); err != nil {
		return err
	}
	log.Printf("OCR completed for video %s: %d sampled segments", video.ID, len(segments))
	return nil
}

func (p *OCRProcessor) deduplicateFrames(ctx context.Context, samples []media.FrameSample) ([]ocrFrame, error) {
	frames := make([]ocrFrame, 0, len(samples))
	for _, sample := range samples {
		// 感知哈希属于本地 CPU 工作，也要在帧之间响应跨进程暂停请求。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 固定采样与场景采样可能命中同一毫秒；时间点相同必须无条件合并，避免生成 [t,t) 区间。
		if len(frames) > 0 && frames[len(frames)-1].TimestampMS == sample.TimestampMS {
			continue
		}
		hash, err := differenceHash(sample.Path)
		if err != nil {
			return nil, fmt.Errorf("hash OCR frame at %d ms: %w", sample.TimestampMS, err)
		}
		// 只合并时间上相邻的近似画面，避免错误删除稍后再次出现的同一张幻灯片。
		if len(frames) > 0 && bits.OnesCount64(frames[len(frames)-1].Hash^hash) <= p.hashDistance {
			continue
		}
		frames = append(frames, ocrFrame{TimestampMS: sample.TimestampMS, Path: sample.Path, Hash: hash})
	}
	return frames, nil
}

func (p *OCRProcessor) recognizeFrames(ctx context.Context, frames []ocrFrame) ([]completedOCRRecognition, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]completedOCRRecognition, len(frames))
	if len(frames) == 0 {
		return results, nil
	}

	tasks := make(chan int)
	errors := make(chan error, 1)
	workerCount := min(p.workers, len(frames))
	var wait sync.WaitGroup
	wait.Add(workerCount)
	for range workerCount {
		go func() {
			defer wait.Done()
			for index := range tasks {
				result, err := p.recognizeWithRetry(workerCtx, frames[index].Path)
				if err != nil {
					select {
					case errors <- err:
					default:
					}
					cancel()
					return
				}
				results[index] = completedOCRRecognition{Result: result, Done: true}
			}
		}()
	}

sendTasks:
	for index := range frames {
		select {
		case tasks <- index:
		case <-workerCtx.Done():
			break sendTasks
		}
	}
	close(tasks)
	wait.Wait()
	select {
	case err := <-errors:
		return results, err
	default:
		return results, ctx.Err()
	}
}

// differenceHash 生成 64 位差值感知哈希，对缩放和轻微压缩变化不敏感。
func differenceHash(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		return 0, err
	}
	bounds := img.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return 0, fmt.Errorf("image has empty bounds")
	}

	var hash uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			left := sampledLuma(img, bounds, x, y)
			right := sampledLuma(img, bounds, x+1, y)
			if left > right {
				hash |= uint64(1) << uint(y*8+x)
			}
		}
	}
	return hash, nil
}

func sampledLuma(img image.Image, bounds image.Rectangle, x, y int) uint32 {
	px := bounds.Min.X + x*(bounds.Dx()-1)/8
	py := bounds.Min.Y + y*(bounds.Dy()-1)/7
	r, g, b, _ := img.At(px, py).RGBA()
	return (299*r + 587*g + 114*b) / 1000
}

func (p *OCRProcessor) recognizeWithRetry(ctx context.Context, imagePath string) (ocrRecognition, error) {
	var lastErr error
	for attempt := 0; attempt < ocrMaxAttempts; attempt++ {
		result, retryable, err := p.recognizeOnce(ctx, imagePath)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return ocrRecognition{}, ctx.Err()
		}
		if !retryable {
			return ocrRecognition{}, err
		}
		lastErr = err
		if attempt == ocrMaxAttempts-1 {
			break
		}
		delay := p.retryDelays[attempt]
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ocrRecognition{}, ctx.Err()
		case <-timer.C:
		}
	}
	return ocrRecognition{}, &ocrServiceUnavailableError{cause: lastErr}
}

func (p *OCRProcessor) recognizeOnce(ctx context.Context, imagePath string) (ocrRecognition, bool, error) {
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return ocrRecognition{}, false, fmt.Errorf("read OCR frame: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"file":                      base64.StdEncoding.EncodeToString(imageBytes),
		"fileType":                  1,
		"useDocOrientationClassify": false,
		"useDocUnwarping":           false,
		"useTextlineOrientation":    false,
		"textRecScoreThresh":        p.minConfidence,
		"visualize":                 false,
	})
	if err != nil {
		return ocrRecognition{}, false, fmt.Errorf("encode OCR request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(payload))
	if err != nil {
		return ocrRecognition{}, false, fmt.Errorf("create OCR request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return ocrRecognition{}, true, fmt.Errorf("call OCR service: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return ocrRecognition{}, true, fmt.Errorf("read OCR response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("OCR service returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError
		return ocrRecognition{}, retryable, err
	}

	var decoded struct {
		ErrorCode int    `json:"errorCode"`
		ErrorMsg  string `json:"errorMsg"`
		Result    struct {
			OCRResults []struct {
				PrunedResult struct {
					Texts  []string  `json:"rec_texts"`
					Scores []float64 `json:"rec_scores"`
				} `json:"prunedResult"`
			} `json:"ocrResults"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ocrRecognition{}, true, fmt.Errorf("decode OCR response: %w", err)
	}
	if decoded.ErrorCode != 0 {
		return ocrRecognition{}, true, fmt.Errorf("OCR service error %d: %s", decoded.ErrorCode, decoded.ErrorMsg)
	}
	if len(decoded.Result.OCRResults) != 1 {
		return ocrRecognition{}, true, fmt.Errorf("OCR service returned %d image results, want 1", len(decoded.Result.OCRResults))
	}
	result := decoded.Result.OCRResults[0].PrunedResult
	if len(result.Texts) != len(result.Scores) {
		return ocrRecognition{}, true, fmt.Errorf("OCR service returned %d texts and %d scores", len(result.Texts), len(result.Scores))
	}
	texts := make([]string, 0, len(result.Texts))
	var confidenceSum float64
	for index, text := range result.Texts {
		text = strings.TrimSpace(text)
		if text == "" || result.Scores[index] < p.minConfidence {
			continue
		}
		texts = append(texts, text)
		confidenceSum += result.Scores[index]
	}
	if len(texts) == 0 {
		return ocrRecognition{}, false, nil
	}
	return ocrRecognition{Text: strings.Join(texts, "\n"), Confidence: confidenceSum / float64(len(texts))}, false, nil
}

func (p *OCRProcessor) replaceOCRSegments(ctx context.Context, videoID string, segments []model.OCRSegment) error {
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// RabbitMQ 与 Outbox 均允许重复投递；原子替换保证任务重跑不会残留旧采样结果。
		if err := tx.Where("video_id = ?", videoID).Delete(&model.OCRSegment{}).Error; err != nil {
			return fmt.Errorf("clear previous OCR segments: %w", err)
		}
		if len(segments) == 0 {
			return nil
		}
		if err := tx.CreateInBatches(segments, 100).Error; err != nil {
			return fmt.Errorf("store OCR segments: %w", err)
		}
		return nil
	})
}
