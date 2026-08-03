package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
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
	asrChunkDuration = 30 * time.Second
)

type audioChunk struct {
	StartMS int64
	EndMS   int64
}

// ASRProcessor 处理 ASR 任务；其他尚未接入的阶段保持原有占位行为。
// client 是复用的官方 OpenAI Go 客户端，连接到配置的兼容服务基地址。
type ASRProcessor struct {
	db      *gorm.DB
	ffmpeg  *media.FFmpeg
	objects *minio.Client
	bucket  string
	client  openai.Client
	model   string
	workers int
}

// NewASRProcessor 创建复用的 OpenAI 客户端，并在 Worker 启动前校验必要连接配置。
func NewASRProcessor(db *gorm.DB, ffmpeg *media.FFmpeg, minIOEndpoint, minIOAccessKey, minIOSecretKey string, minIOSecure bool, bucket, baseURL, apiKey, modelName string, concurrency int) (*ASRProcessor, error) {
	if db == nil || ffmpeg == nil {
		return nil, fmt.Errorf("ASR processor requires database and ffmpeg")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("ASR processor requires MinIO bucket")
	}
	endpointURL, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || (endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Host == "" {
		return nil, fmt.Errorf("ASR_BASE_URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("ASR_API_KEY is required")
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, fmt.Errorf("ASR_MODEL is required")
	}
	if concurrency < 1 {
		return nil, fmt.Errorf("ASR_CONCURRENCY must be positive")
	}

	objects, err := minio.New(minIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minIOAccessKey, minIOSecretKey, ""),
		Secure: minIOSecure,
	})
	if err != nil {
		return nil, fmt.Errorf("create MinIO client for ASR: %w", err)
	}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(endpointURL.String()),
		// 为每次 ASR 请求设置上限，避免下游服务异常时长期占用 RabbitMQ delivery。
		option.WithHTTPClient(&http.Client{Timeout: 4 * time.Minute}),
	)
	return &ASRProcessor{
		db: db, ffmpeg: ffmpeg, objects: objects, bucket: bucket,
		client: client, model: modelName, workers: concurrency,
	}, nil
}

// Process 实现 Processor 接口，路由器已按 TaskType 过滤，此处直接执行 ASR。
func (p *ASRProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	return p.consumeASR(ctx, job)
}

// consumeASR 下载对象存储中的视频、提取音频并转写，再以视频 0 ms 为基准保存 ASR 片段。
func (p *ASRProcessor) consumeASR(ctx context.Context, job queue.VideoJobMessage) error {
	var video model.Video
	if err := p.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", job.VideoID, job.TenantID).First(&video).Error; err != nil {
		return fmt.Errorf("load ASR video %s: %w", job.VideoID, err)
	}
	if strings.TrimSpace(video.ObjectKey) == "" {
		return fmt.Errorf("ASR video %s has no object key", video.ID)
	}

	workDir, err := os.MkdirTemp("", "readergo-asr-*")
	if err != nil {
		return fmt.Errorf("create ASR work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	// 原始视频只落在任务独占的临时目录；完成或失败后统一清理，避免占用 Worker 磁盘。
	videoPath := filepath.Join(workDir, "input-video")
	if err := p.objects.FGetObject(ctx, p.bucket, video.ObjectKey, videoPath, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download video object %q: %w", video.ObjectKey, err)
	}
	info, err := p.ffmpeg.Probe(ctx, videoPath)
	if err != nil {
		return err
	}
	if info.DurationMS <= 0 {
		return fmt.Errorf("video %s has invalid duration %d ms", video.ID, info.DurationMS)
	}
	if info.AudioCodec == "" {
		return fmt.Errorf("video %s does not contain an audio stream", video.ID)
	}
	if err := p.db.WithContext(ctx).Model(&model.Video{}).Where("id = ?", video.ID).Update("duration_ms", info.DurationMS).Error; err != nil {
		return fmt.Errorf("store video duration: %w", err)
	}

	chunks := buildAudioChunks(info.DurationMS)
	transcripts, err := p.transcribeChunks(ctx, videoPath, workDir, chunks)
	if err != nil {
		return err
	}
	segments := make([]model.ASRSegment, 0, len(chunks))
	for index, chunk := range chunks {
		transcript := transcripts[index]
		if transcript != "" {
			segments = append(segments, model.ASRSegment{TenantID: video.TenantID, VideoID: video.ID, StartMS: chunk.StartMS, EndMS: chunk.EndMS, Transcript: transcript})
		}
	}
	if err := p.replaceASRSegments(ctx, video.ID, segments); err != nil {
		return err
	}
	log.Printf("ASR completed for video %s: %d segments", video.ID, len(segments))
	return nil
}

func (p *ASRProcessor) transcribeChunks(ctx context.Context, videoPath, workDir string, chunks []audioChunk) ([]string, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	transcripts := make([]string, len(chunks))
	tasks := make(chan int)
	errors := make(chan error, 1)
	workerCount := min(p.workers, len(chunks))
	var wait sync.WaitGroup
	wait.Add(workerCount)
	for range workerCount {
		go func() {
			defer wait.Done()
			for index := range tasks {
				chunk := chunks[index]
				audioPath := filepath.Join(workDir, fmt.Sprintf("audio-%06d.wav", chunk.StartMS))
				// 每个协程使用独立文件，提取区间与最终落库时间严格一致。
				if err := p.ffmpeg.ExtractAudioSegment(workerCtx, videoPath, audioPath, time.Duration(chunk.StartMS)*time.Millisecond, time.Duration(chunk.EndMS)*time.Millisecond); err != nil {
					select {
					case errors <- err:
					default:
					}
					cancel()
					return
				}
				transcript, err := p.transcribeAudio(workerCtx, audioPath)
				if err != nil {
					select {
					case errors <- err:
					default:
					}
					cancel()
					return
				}
				transcripts[index] = transcript
			}
		}()
	}

sendTasks:
	for index := range chunks {
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
		return nil, err
	default:
		return transcripts, ctx.Err()
	}
}

// transcribeAudio 使用 OpenAI 客户端调用兼容的 Chat Completions ASR 接口。
func (p *ASRProcessor) transcribeAudio(ctx context.Context, audioPath string) (string, error) {
	audioBytes, err := os.ReadFile(audioPath)
	if err != nil {
		return "", fmt.Errorf("read extracted audio: %w", err)
	}
	// ASR 服务使用 data URL；保留 WAV 格式声明，确保兼容客户端和服务端都能识别载荷。
	audioData := "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(audioBytes)
	completion, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{
					Data:   audioData,
					Format: "wav",
				}),
			}),
		},
	}, option.WithJSONSet("asr_options", map[string]string{"language": "zh"}))
	if err != nil {
		return "", fmt.Errorf("call ASR chat completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("ASR service returned no completion choices")
	}
	return strings.TrimSpace(completion.Choices[0].Message.Content), nil
}

// buildAudioChunks 从视频 0 ms 起按固定 30 秒切分，生成连续的左闭右开区间；
// 最后不足 30 秒的内容单独保留，确保整条视频时间轴既不遗漏也不重叠。
func buildAudioChunks(durationMS int64) []audioChunk {
	if durationMS <= 0 {
		return nil
	}

	chunkDurationMS := asrChunkDuration.Milliseconds()
	chunks := make([]audioChunk, 0, (durationMS+chunkDurationMS-1)/chunkDurationMS)
	for startMS := int64(0); startMS < durationMS; startMS += chunkDurationMS {
		endMS := min(startMS+chunkDurationMS, durationMS)
		chunks = append(chunks, audioChunk{StartMS: startMS, EndMS: endMS})
	}
	return chunks
}

// replaceASRSegments 原子替换 ASR 原始结果；RAG 稍后据此和 Vision 原始结果动态构建 video_segments。
func (p *ASRProcessor) replaceASRSegments(ctx context.Context, videoID string, segments []model.ASRSegment) error {
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 任务在写入后、标记成功前中断时会重试；先删除再插入可避免产生重复原始片段。
		if err := tx.Where("video_id = ?", videoID).Delete(&model.ASRSegment{}).Error; err != nil {
			return fmt.Errorf("clear previous ASR segments: %w", err)
		}
		if len(segments) == 0 {
			return nil
		}
		if err := tx.CreateInBatches(segments, 100).Error; err != nil {
			return fmt.Errorf("store ASR segments: %w", err)
		}
		return nil
	})
}
