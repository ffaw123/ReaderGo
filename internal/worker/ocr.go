package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
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
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

const (
	ocrFixedSampleInterval = 15 * time.Second
	ocrMaxRetries          = 2
	ocrRequestTimeout      = 60 * time.Second
)

const ocrUserPrompt = "Please output only the text content from the image without any additional descriptions or formatting."

type ocrFrame struct {
	TimestampMS int64
	Path        string
	Hash        uint64
}

// OCRProcessor 实现现有 vision 阶段：抽取视频画面后调用独立 OCR 模型，并保存可用的原始文字。
type OCRProcessor struct {
	db             *gorm.DB
	ffmpeg         *media.FFmpeg
	objects        *minio.Client
	bucket         string
	client         openai.Client
	model          string
	sceneThreshold float64
	hashDistance   int
	workers        int
}

// NewOCRProcessor 创建独立于 ASR 和 RAG 的 OpenAI 兼容客户端，并校验 OCR 模型配置。
func NewOCRProcessor(db *gorm.DB, ffmpeg *media.FFmpeg, minIOEndpoint, minIOAccessKey, minIOSecretKey string, minIOSecure bool, bucket, baseURL, apiKey, modelName string, sceneThreshold float64, hashDistance, concurrency int) (*OCRProcessor, error) {
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
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OCR_API_KEY is required")
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, fmt.Errorf("OCR_MODEL is required")
	}
	if sceneThreshold <= 0 || sceneThreshold > 1 {
		return nil, fmt.Errorf("OCR scene threshold must be greater than 0 and at most 1")
	}
	if hashDistance < 0 || hashDistance > 64 {
		return nil, fmt.Errorf("OCR hash distance must be between 0 and 64")
	}
	if concurrency < 1 || concurrency > 10 {
		return nil, fmt.Errorf("OCR concurrency must be between 1 and 10")
	}

	objects, err := minio.New(minIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minIOAccessKey, minIOSecretKey, ""),
		Secure: minIOSecure,
	})
	if err != nil {
		return nil, fmt.Errorf("create MinIO client for OCR: %w", err)
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)),
		option.WithBaseURL(endpointURL.String()),
		option.WithHTTPClient(&http.Client{Timeout: ocrRequestTimeout}),
		// 每个抽样帧在 SDK 内处理临时 HTTP 错误，业务层只接收最终成功或失败结果。
		option.WithMaxRetries(ocrMaxRetries),
	)
	return &OCRProcessor{
		db: db, ffmpeg: ffmpeg, objects: objects, bucket: bucket,
		client: client, model: strings.TrimSpace(modelName),
		sceneThreshold: sceneThreshold, hashDistance: hashDistance, workers: concurrency,
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

	results, err := p.recognizeFrames(ctx, frames)
	if err != nil {
		// 不再用数据库 status 表达服务异常；任一请求失败时让任务整体重试，避免提交不完整时间轴。
		return err
	}
	segments := buildOCRSegments(video, info.DurationMS, frames, results)
	if err := p.replaceOCRSegments(ctx, video.ID, segments); err != nil {
		return err
	}
	log.Printf("OCR completed for video %s: %d usable segments from %d sampled frames", video.ID, len(segments), len(frames))
	return nil
}

func buildOCRSegments(video model.Video, durationMS int64, frames []ocrFrame, results []string) []model.OCRSegment {
	segments := make([]model.OCRSegment, 0, len(frames))
	for index, frame := range frames {
		if index >= len(results) {
			break
		}
		text := strings.TrimSpace(results[index])
		// Qwen-VL-OCR 不返回置信度；只有空文本不代表有效视频证据，直接丢弃且不写入占位记录。
		if text == "" {
			continue
		}
		endMS := durationMS
		if index+1 < len(frames) {
			endMS = frames[index+1].TimestampMS
		}
		if endMS <= frame.TimestampMS {
			continue
		}
		segments = append(segments, model.OCRSegment{
			TenantID: video.TenantID, VideoID: video.ID,
			StartMS: frame.TimestampMS, EndMS: endMS,
			Text: text,
		})
	}
	return segments
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

func (p *OCRProcessor) recognizeFrames(ctx context.Context, frames []ocrFrame) ([]string, error) {
	results := make([]string, len(frames))
	if len(frames) == 0 {
		return results, nil
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tasks := make(chan int)
	errorsChannel := make(chan error, 1)
	workerCount := min(p.workers, len(frames))
	var wait sync.WaitGroup
	wait.Add(workerCount)
	for range workerCount {
		go func() {
			defer wait.Done()
			for index := range tasks {
				result, err := p.recognizeImage(workerCtx, frames[index].Path)
				if err != nil {
					select {
					case errorsChannel <- fmt.Errorf("recognize OCR frame at %d ms: %w", frames[index].TimestampMS, err):
						cancel()
					default:
					}
					return
				}
				results[index] = result
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
	case err := <-errorsChannel:
		return nil, err
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

func (p *OCRProcessor) recognizeImage(ctx context.Context, imagePath string) (string, error) {
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("read OCR frame: %w", err)
	}
	imageDataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	completion, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: imageDataURL,
				}),
				openai.TextContentPart(ocrUserPrompt),
			}),
		},
	})
	if err != nil {
		return "", fmt.Errorf("call OCR chat completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("OCR model returned no completion choices")
	}
	// Qwen-VL-OCR 返回纯文本；除落库前去除首尾空白外，不做语言、长度或内容规则校验。
	return completion.Choices[0].Message.Content, nil
}

func (p *OCRProcessor) replaceOCRSegments(ctx context.Context, videoID string, segments []model.OCRSegment) error {
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// RabbitMQ 与 Outbox 均允许重复投递；原子替换保证任务重跑不会残留旧识别结果。
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
