package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"readergo/internal/model"
	"readergo/internal/queue"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

const (
	ragEmbeddingDimensions = 1024
	ragEmbeddingBatchSize  = 10
)

// EmbeddingProcessor 只负责把已落库的 RAG 检索文本转换为向量。
// 它不参与窗口生成，使向量服务故障不会阻止 video_segments 和视频摘要落库。
type EmbeddingProcessor struct {
	db             *gorm.DB
	client         openai.Client
	embeddingModel string
}

func NewEmbeddingProcessor(db *gorm.DB, baseURL, apiKey, embeddingModel string, requestTimeout time.Duration) (*EmbeddingProcessor, error) {
	if db == nil {
		return nil, fmt.Errorf("embedding processor requires database")
	}
	endpointURL, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || (endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Host == "" {
		return nil, fmt.Errorf("RAG_BASE_URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("RAG_API_KEY is required")
	}
	if strings.TrimSpace(embeddingModel) == "" {
		return nil, fmt.Errorf("RAG_EMBEDDING_MODEL is required")
	}
	if requestTimeout <= 0 {
		return nil, fmt.Errorf("RAG_REQUEST_TIMEOUT must be positive")
	}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(endpointURL.String()),
		option.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		option.WithMaxRetries(0),
	)
	return &EmbeddingProcessor{db: db, client: client, embeddingModel: strings.TrimSpace(embeddingModel)}, nil
}

func (p *EmbeddingProcessor) Process(ctx context.Context, job queue.VideoJobMessage) error {
	var segments []model.VideoSegment
	if err := p.db.WithContext(ctx).
		Where("tenant_id = ? AND video_id = ?", job.TenantID, job.VideoID).
		Order("start_ms ASC, end_ms ASC").Find(&segments).Error; err != nil {
		return fmt.Errorf("load RAG segments for embedding video %s: %w", job.VideoID, err)
	}
	if len(segments) == 0 {
		// rag_index 已成功却没有片段属于数据契约异常，保留任务级重试和最终 DLQ。
		return fmt.Errorf("video %s has no RAG segments to embed", job.VideoID)
	}

	vectors, err := p.embedSegments(ctx, segments)
	if err != nil {
		return err
	}
	if err := p.replaceEmbeddings(ctx, job, segments, vectors); err != nil {
		return err
	}
	log.Printf("embedding completed for video %s: %d vectors", job.VideoID, len(vectors))
	return nil
}

// embedSegments 按每批最多 10 个片段并发请求，输出始终与输入片段顺序对齐。
func (p *EmbeddingProcessor) embedSegments(ctx context.Context, segments []model.VideoSegment) ([][]float64, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	vectors := make([][]float64, len(segments))
	batchCount := (len(segments) + ragEmbeddingBatchSize - 1) / ragEmbeddingBatchSize
	errorsChannel := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(batchCount)
	for batchIndex, start := 0, 0; start < len(segments); batchIndex, start = batchIndex+1, start+ragEmbeddingBatchSize {
		end := min(start+ragEmbeddingBatchSize, len(segments))
		go func(batchIndex, start, end int) {
			defer wait.Done()
			batchVectors, err := p.embedBatchWithRetry(workerCtx, batchIndex+1, segments[start:end])
			if err != nil {
				if workerCtx.Err() != nil && ctx.Err() == nil {
					return
				}
				select {
				case errorsChannel <- fmt.Errorf("embed batch %d [%d:%d]: %w", batchIndex+1, start, end, err):
					cancel()
				default:
				}
				return
			}
			copy(vectors[start:end], batchVectors)
		}(batchIndex, start, end)
	}
	wait.Wait()
	select {
	case err := <-errorsChannel:
		return nil, err
	default:
		return vectors, ctx.Err()
	}
}

func (p *EmbeddingProcessor) embedBatchWithRetry(ctx context.Context, batchIndex int, segments []model.VideoSegment) ([][]float64, error) {
	for attempt := 0; ; attempt++ {
		vectors, retryable, err := p.embedBatchOnce(ctx, segments)
		if err == nil {
			return vectors, nil
		}
		if !retryable {
			return nil, err
		}
		if attempt >= ragMaxOutputRetries {
			return nil, fmt.Errorf("embedding batch %d output remains invalid after %d attempts: %w", batchIndex, attempt+1, err)
		}
		delay := ragOutputRetryDelays[attempt]
		log.Printf("embedding batch %d output attempt %d/%d invalid, retrying in %s: %v", batchIndex, attempt+1, ragMaxOutputRetries+1, delay, err)
		if err := waitRAGOutputRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (p *EmbeddingProcessor) embedBatchOnce(ctx context.Context, segments []model.VideoSegment) ([][]float64, bool, error) {
	inputs := make([]string, len(segments))
	for index, segment := range segments {
		inputs[index] = segment.RetrievalText
	}
	response, err := p.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model:      openai.EmbeddingModel(p.embeddingModel),
		Input:      openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
		Dimensions: openai.Int(ragEmbeddingDimensions),
	}, option.WithMaxRetries(ragMaxRetries))
	if err != nil {
		if isEmbeddingResponseDecodeError(err) {
			return nil, true, fmt.Errorf("decode embedding response: %w", err)
		}
		// HTTP 和网络错误已经由 SDK 完成重试，避免和输出校验重试相乘。
		return nil, false, fmt.Errorf("call embedding service: %w", err)
	}
	if len(response.Data) != len(segments) {
		return nil, true, fmt.Errorf("embedding service returned %d vectors, want %d", len(response.Data), len(segments))
	}
	vectors := make([][]float64, len(segments))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= int64(len(segments)) || vectors[item.Index] != nil {
			return nil, true, fmt.Errorf("embedding service returned invalid or duplicate index %d", item.Index)
		}
		if len(item.Embedding) != ragEmbeddingDimensions {
			// 向量维度由模型配置和数据库 vector(1024) 契约共同决定，重复执行任务无法自行恢复。
			return nil, false, permanentProcessError(
				fmt.Errorf("embedding %d has %d dimensions, want %d", item.Index, len(item.Embedding), ragEmbeddingDimensions),
			)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, false, nil
}

func isEmbeddingResponseDecodeError(err error) bool {
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	return errors.As(err, &syntaxError) || errors.As(err, &typeError) || errors.Is(err, io.ErrUnexpectedEOF)
}

// replaceEmbeddings 在全部向量校验完成后原子替换，失败不会留下半批数据。
func (p *EmbeddingProcessor) replaceEmbeddings(ctx context.Context, job queue.VideoJobMessage, segments []model.VideoSegment, vectors [][]float64) error {
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("video_id = ?", job.VideoID).Delete(&model.SegmentEmbedding{}).Error; err != nil {
			return fmt.Errorf("clear previous embeddings: %w", err)
		}
		for index, segment := range segments {
			vector, err := formatPGVector(vectors[index])
			if err != nil {
				return fmt.Errorf("format embedding for segment %s: %w", segment.ID, err)
			}
			// 显式转换为 pgvector，避免驱动把字符串按 text 或 JSON 类型写入。
			if err := tx.Exec(`INSERT INTO segment_embeddings
				(id, tenant_id, video_id, segment_id, embedding_model, embedding, created_at)
				VALUES (?, ?, ?, ?, ?, ?::vector, ?)`,
				uuid.NewString(), job.TenantID, job.VideoID, segment.ID, p.embeddingModel, vector, time.Now().UTC()).Error; err != nil {
				return fmt.Errorf("store embedding for segment %s: %w", segment.ID, err)
			}
		}
		return nil
	})
}

func formatPGVector(vector []float64) (string, error) {
	if len(vector) != ragEmbeddingDimensions {
		return "", fmt.Errorf("vector has %d dimensions, want %d", len(vector), ragEmbeddingDimensions)
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("vector contains a non-finite value at index %d", index)
		}
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	}
	builder.WriteByte(']')
	return builder.String(), nil
}
