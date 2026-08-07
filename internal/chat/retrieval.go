package chat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"readergo/internal/model"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

const (
	queryEmbeddingDimensions = 1024
	retrievalChannelTopK     = 8
	retrievalFinalTopK       = 5
	retrievalRRFConstant     = 60.0
	maxContextWindows        = 10
)

type retriever struct {
	db              *gorm.DB
	embeddingClient openai.Client
	embeddingModel  string
	minSimilarity   float64
}

type semanticHit struct {
	SegmentID  string
	Similarity float64
}

type segmentCandidate struct {
	Segment   model.VideoSegment
	DenseRank int
	ExactRank int
	BM25Rank  int
	RRFScore  float64
	Primary   bool
}

type retrievalResult struct {
	Candidates []segmentCandidate
}

func newRetriever(db *gorm.DB, baseURL, apiKey, modelName string, timeout time.Duration, minSimilarity float64) (*retriever, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("RAG_BASE_URL must be an absolute HTTP(S) URL for chat retrieval")
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelName) == "" {
		return nil, errors.New("RAG_API_KEY and RAG_EMBEDDING_MODEL are required for chat retrieval")
	}
	if timeout <= 0 || minSimilarity < -1 || minSimilarity > 1 {
		return nil, errors.New("chat retrieval timeout or similarity threshold is invalid")
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)), option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}), option.WithMaxRetries(2),
	)
	return &retriever{db: db, embeddingClient: client, embeddingModel: strings.TrimSpace(modelName), minSimilarity: minSimilarity}, nil
}

func (r *retriever) Retrieve(ctx context.Context, tenantID, videoID, query string) (retrievalResult, error) {
	segments, err := r.loadSegments(ctx, tenantID, videoID)
	if err != nil {
		return retrievalResult{}, err
	}
	documents := buildSegmentTextDocuments(segments)
	vector, err := r.embedQuery(ctx, query)
	if err != nil {
		return retrievalResult{}, err
	}
	dense, err := r.semanticSearch(ctx, tenantID, videoID, vector)
	if err != nil {
		return retrievalResult{}, err
	}
	// 在线检索直接使用 video_segments 中的 ASR/OCR 快照，避免重复读取和映射原始结果表。
	exact := rankExact(query, documents, 0)
	bm25 := rankBM25(query, documents, 0)
	candidates := fuseRetrievalChannels(segments, dense, exact, bm25)
	candidates = expandNeighborWindows(segments, candidates)
	return retrievalResult{Candidates: candidates}, nil
}

func (r *retriever) loadSegments(ctx context.Context, tenantID, videoID string) ([]model.VideoSegment, error) {
	var segments []model.VideoSegment
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND video_id = ?", tenantID, videoID).
		Order("start_ms ASC, end_ms ASC, id ASC").Find(&segments).Error; err != nil {
		return nil, fmt.Errorf("load retrieval windows: %w", err)
	}
	return segments, nil
}

func buildSegmentTextDocuments(segments []model.VideoSegment) []segmentTextDocument {
	documents := make([]segmentTextDocument, 0, len(segments)*2)
	for _, segment := range segments {
		if text := strings.TrimSpace(segment.Transcript); text != "" {
			documents = append(documents, segmentTextDocument{SegmentID: segment.ID, SourceType: "asr", Text: text})
		}
		if text := strings.TrimSpace(segment.OCRText); text != "" {
			documents = append(documents, segmentTextDocument{SegmentID: segment.ID, SourceType: "ocr", Text: text})
		}
	}
	return documents
}

func (r *retriever) embedQuery(ctx context.Context, query string) ([]float64, error) {
	response, err := r.embeddingClient.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model:      openai.EmbeddingModel(r.embeddingModel),
		Input:      openai.EmbeddingNewParamsInputUnion{OfString: openai.String(query)},
		Dimensions: openai.Int(queryEmbeddingDimensions),
	}, option.WithMaxRetries(2))
	if err != nil {
		return nil, fmt.Errorf("embed retrieval query: %w", err)
	}
	if len(response.Data) != 1 || len(response.Data[0].Embedding) != queryEmbeddingDimensions {
		return nil, fmt.Errorf("query embedding response has invalid dimensions")
	}
	return response.Data[0].Embedding, nil
}

func (r *retriever) semanticSearch(ctx context.Context, tenantID, videoID string, embedding []float64) ([]semanticHit, error) {
	vector, err := formatVector(embedding)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		SegmentID  string  `gorm:"column:segment_id"`
		Similarity float64 `gorm:"column:similarity"`
	}
	if err := r.db.WithContext(ctx).Raw(`
		SELECT segment_id, 1 - (embedding <=> ?::vector) AS similarity
		FROM segment_embeddings
		WHERE tenant_id = ? AND video_id = ?
		ORDER BY embedding <=> ?::vector
		LIMIT ?`, vector, tenantID, videoID, vector, retrievalChannelTopK).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query semantic candidates: %w", err)
	}
	hits := make([]semanticHit, 0, len(rows))
	for _, row := range rows {
		if row.Similarity >= r.minSimilarity {
			hits = append(hits, semanticHit{SegmentID: row.SegmentID, Similarity: row.Similarity})
		}
	}
	return hits, nil
}

func formatVector(vector []float64) (string, error) {
	if len(vector) != queryEmbeddingDimensions {
		return "", fmt.Errorf("vector has %d dimensions, want %d", len(vector), queryEmbeddingDimensions)
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("vector contains non-finite value at %d", index)
		}
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	}
	builder.WriteByte(']')
	return builder.String(), nil
}

func fuseRetrievalChannels(segments []model.VideoSegment, dense []semanticHit, exact, bm25 []rankedSegmentText) []segmentCandidate {
	candidates := make(map[string]*segmentCandidate, len(segments))
	segmentByID := make(map[string]model.VideoSegment, len(segments))
	for _, segment := range segments {
		segmentByID[segment.ID] = segment
	}
	ensure := func(segment model.VideoSegment) *segmentCandidate {
		candidate := candidates[segment.ID]
		if candidate == nil {
			candidate = &segmentCandidate{Segment: segment, Primary: true}
			candidates[segment.ID] = candidate
		}
		return candidate
	}
	for index, hit := range dense {
		if segment, ok := segmentByID[hit.SegmentID]; ok {
			ensure(segment).DenseRank = index + 1
		}
	}
	exactWindowRanks := segmentWindowRanks(segments, exact, retrievalChannelTopK)
	for segmentID, rank := range exactWindowRanks {
		segment := segmentByID[segmentID]
		candidate := ensure(segment)
		if candidate.ExactRank == 0 || rank < candidate.ExactRank {
			candidate.ExactRank = rank
		}
	}
	bm25WindowRanks := segmentWindowRanks(segments, bm25, retrievalChannelTopK)
	for segmentID, rank := range bm25WindowRanks {
		segment := segmentByID[segmentID]
		candidate := ensure(segment)
		if candidate.BM25Rank == 0 || rank < candidate.BM25Rank {
			candidate.BM25Rank = rank
		}
	}

	result := make([]segmentCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.DenseRank > 0 {
			candidate.RRFScore += 1 / (retrievalRRFConstant + float64(candidate.DenseRank))
		}
		if candidate.ExactRank > 0 {
			candidate.RRFScore += 1 / (retrievalRRFConstant + float64(candidate.ExactRank))
		}
		if candidate.BM25Rank > 0 {
			candidate.RRFScore += 1 / (retrievalRRFConstant + float64(candidate.BM25Rank))
		}
		result = append(result, *candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].RRFScore == result[j].RRFScore {
			return result[i].Segment.StartMS < result[j].Segment.StartMS
		}
		return result[i].RRFScore > result[j].RRFScore
	})
	if len(result) > retrievalFinalTopK {
		result = result[:retrievalFinalTopK]
	}
	return result
}

func segmentWindowRanks(segments []model.VideoSegment, hits []rankedSegmentText, limit int) map[string]int {
	bestScores := make(map[string]float64)
	for _, hit := range hits {
		// 同一窗口的 ASR 和 OCR 独立评分，窗口排名只采用其中更高的得分。
		if hit.Score > bestScores[hit.Document.SegmentID] {
			bestScores[hit.Document.SegmentID] = hit.Score
		}
	}
	type scoredWindow struct {
		ID      string
		Score   float64
		StartMS int64
	}
	ranked := make([]scoredWindow, 0, len(bestScores))
	for _, segment := range segments {
		if score := bestScores[segment.ID]; score > 0 {
			ranked = append(ranked, scoredWindow{ID: segment.ID, Score: score, StartMS: segment.StartMS})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].StartMS < ranked[j].StartMS
		}
		return ranked[i].Score > ranked[j].Score
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	ranks := make(map[string]int, len(ranked))
	currentRank := 0
	for index, window := range ranked {
		if index == 0 || window.Score != ranked[index-1].Score {
			currentRank = index + 1
		}
		// 得分相同的窗口共享排名，时间顺序只用于保证结果稳定。
		ranks[window.ID] = currentRank
	}
	return ranks
}

func expandNeighborWindows(all []model.VideoSegment, primary []segmentCandidate) []segmentCandidate {
	if len(primary) == 0 {
		return nil
	}
	indexByID := make(map[string]int, len(all))
	for index, segment := range all {
		indexByID[segment.ID] = index
	}
	result := append([]segmentCandidate(nil), primary...)
	seen := make(map[string]struct{}, len(result))
	for _, candidate := range result {
		seen[candidate.Segment.ID] = struct{}{}
	}
	for _, candidate := range primary {
		center := indexByID[candidate.Segment.ID]
		for _, neighborIndex := range []int{center - 1, center + 1} {
			if neighborIndex < 0 || neighborIndex >= len(all) || len(result) >= maxContextWindows {
				continue
			}
			segment := all[neighborIndex]
			if _, ok := seen[segment.ID]; ok {
				continue
			}
			seen[segment.ID] = struct{}{}
			result = append(result, segmentCandidate{Segment: segment, Primary: false})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Segment.StartMS < result[j].Segment.StartMS })
	return result
}
