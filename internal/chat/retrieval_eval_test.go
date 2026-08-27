package chat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/model"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"gorm.io/gorm"
)

const (
	retrievalEvalRunEnv      = "RUN_RETRIEVAL_EVAL"
	retrievalEvalDatabaseEnv = "RETRIEVAL_EVAL_DATABASE_URL"
	retrievalEvalDatasetEnv  = "RETRIEVAL_EVAL_DATASET"
	retrievalEvalSnapshotEnv = "RETRIEVAL_EVAL_DATABASE_SNAPSHOT"
	retrievalEvalResultsEnv  = "RETRIEVAL_EVAL_RESULTS"

	// 这些常量是业务检索参数的评测快照。测试代码不引用业务常量，便于独立做消融实验。
	evalEmbeddingDimensions = 1024
	evalChannelTopK         = 8
	evalFinalTopK           = 5
	evalFusionRRFConstant   = 10.0
	evalResultSchemaVersion = "retrieval-eval-run-v4"
)

var evalLexicalStopTokens = map[string]struct{}{
	"这个": {}, "那个": {}, "视频": {}, "里面": {}, "作者": {}, "什么": {}, "怎么": {},
	"为什么": {}, "是否": {}, "有没有": {}, "提到": {}, "一下": {}, "请问": {}, "关于": {},
}

type evalQuestion struct {
	QuestionID         string     `json:"question_id"`
	TenantID           string     `json:"tenant_id"`
	VideoID            string     `json:"video_id"`
	VideoTitle         string     `json:"video_title,omitempty"`
	Question           string     `json:"question"`
	Answer             string     `json:"answer,omitempty"`
	Tags               []string   `json:"tags,omitempty"`
	GoldEvidenceGroups [][]string `json:"gold_evidence_groups"`
}

type evalMetrics struct {
	RecallAt5 float64 `json:"recall_at_5"`
	RRAt5     float64 `json:"rr_at_5"`
}

type evalAggregate struct {
	QuestionCount int
	RecallSum     float64
	RRSum         float64
}

type evalRRFWeights struct {
	Dense float64 `json:"dense"`
	BM25  float64 `json:"bm25"`
}

type evalExperimentSpec struct {
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Weights          evalRRFWeights       `json:"weights"`
	RRFConstant      float64              `json:"rrf_constant"`
	BM25Source       string               `json:"bm25_source,omitempty"`
	BM25FieldWeights evalBM25FieldWeights `json:"bm25_field_weights,omitempty"`
}

type evalChannelCandidateResult struct {
	Rank      int     `json:"rank"`
	SegmentID string  `json:"segment_id"`
	StartMS   int64   `json:"start_ms"`
	EndMS     int64   `json:"end_ms"`
	Score     float64 `json:"score"`
}

type evalChannelResults struct {
	Dense []evalChannelCandidateResult `json:"dense_top_8"`
	BM25  []evalChannelCandidateResult `json:"bm25_top_8"`
}

type evalCandidateResult struct {
	ResultRank        int     `json:"result_rank"`
	SegmentID         string  `json:"segment_id"`
	StartMS           int64   `json:"start_ms"`
	EndMS             int64   `json:"end_ms"`
	DenseRank         int     `json:"dense_rank"`
	DenseScore        float64 `json:"dense_score"`
	BM25Rank          int     `json:"bm25_rank"`
	BM25Score         float64 `json:"bm25_score"`
	DenseContribution float64 `json:"dense_rrf_contribution"`
	BM25Contribution  float64 `json:"bm25_rrf_contribution"`
	RRFScore          float64 `json:"rrf_score"`
}

type evalQuestionExperimentResult struct {
	Name       string                `json:"name"`
	Metrics    evalMetrics           `json:"metrics"`
	Candidates []evalCandidateResult `json:"candidates"`
}

type evalQuestionResult struct {
	QuestionID         string                         `json:"question_id"`
	TenantID           string                         `json:"tenant_id"`
	VideoID            string                         `json:"video_id"`
	VideoTitle         string                         `json:"video_title,omitempty"`
	Question           string                         `json:"question"`
	Answer             string                         `json:"answer,omitempty"`
	Tags               []string                       `json:"tags,omitempty"`
	GoldEvidenceGroups [][]string                     `json:"gold_evidence_groups"`
	Channels           evalChannelResults             `json:"channels"`
	Experiments        []evalQuestionExperimentResult `json:"experiments"`
}

type evalExperimentSummary struct {
	Name                     string               `json:"name"`
	Description              string               `json:"description"`
	Weights                  evalRRFWeights       `json:"weights"`
	RRFConstant              float64              `json:"rrf_constant"`
	BM25Source               string               `json:"bm25_source,omitempty"`
	BM25FieldWeights         evalBM25FieldWeights `json:"bm25_field_weights,omitempty"`
	Metrics                  evalMetrics          `json:"metrics"`
	RecallDeltaVsDensePoints float64              `json:"recall_delta_vs_dense_points"`
	MRRDeltaVsDensePoints    float64              `json:"mrr_delta_vs_dense_points"`
}

type evalRunConfig struct {
	DatasetPath            string  `json:"dataset_path"`
	DatasetSHA256          string  `json:"dataset_sha256"`
	DatabaseSnapshotPath   string  `json:"database_snapshot_path"`
	DatabaseSnapshotSHA256 string  `json:"database_snapshot_sha256"`
	DatabaseIdentity       string  `json:"database_identity"`
	EmbeddingModel         string  `json:"embedding_model"`
	MinSimilarity          float64 `json:"min_similarity"`
	ChannelTopK            int     `json:"channel_top_k"`
	FinalTopK              int     `json:"final_top_k"`
}

type evalRunRecord struct {
	SchemaVersion string                  `json:"schema_version"`
	RunID         string                  `json:"run_id"`
	StartedAt     time.Time               `json:"started_at"`
	CompletedAt   time.Time               `json:"completed_at"`
	QuestionCount int                     `json:"question_count"`
	Config        evalRunConfig           `json:"config"`
	Experiments   []evalExperimentSummary `json:"experiments"`
	Questions     []evalQuestionResult    `json:"questions"`
}

type evalRetriever struct {
	db              *gorm.DB
	embeddingClient openai.Client
	embeddingModel  string
	minSimilarity   float64
}

type evalDenseHit struct {
	SegmentID  string
	Similarity float64
}

type evalTextDocument struct {
	SegmentID  string
	SourceType string
	Text       string
}

type evalRankedText struct {
	Document evalTextDocument
	Score    float64
}

type evalWindowHit struct {
	SegmentID string
	Rank      int
	Score     float64
}

type evalCandidate struct {
	Segment           model.VideoSegment
	DenseRank         int
	DenseScore        float64
	BM25Rank          int
	BM25Score         float64
	DenseContribution float64
	BM25Contribution  float64
	RRFScore          float64
}

type evalRawChannels struct {
	Segments []model.VideoSegment
	Dense    []evalDenseHit
	BM25     []evalWindowHit
}

func (a *evalAggregate) Add(metrics evalMetrics) {
	a.QuestionCount++
	a.RecallSum += metrics.RecallAt5
	a.RRSum += metrics.RRAt5
}

func (a evalAggregate) Metrics() evalMetrics {
	if a.QuestionCount == 0 {
		return evalMetrics{}
	}
	return evalMetrics{RecallAt5: a.RecallSum / float64(a.QuestionCount), RRAt5: a.RRSum / float64(a.QuestionCount)}
}

func evalExperimentSpecs() []evalExperimentSpec {
	return []evalExperimentSpec{
		{Name: "dense_only", Description: "E1 仅使用 Dense Vector 的主基线", Weights: evalRRFWeights{Dense: 1}},
		{Name: "bm25_only", Description: "E2 仅使用 BM25 的词法检索基线", Weights: evalRRFWeights{BM25: 1}, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_equal", Description: "E3 Dense + BM25 等权融合", Weights: evalRRFWeights{Dense: 1, BM25: 1}, RRFConstant: evalFusionRRFConstant, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_0_5", Description: "E4 中等 BM25 权重", Weights: evalRRFWeights{Dense: 1, BM25: 0.5}, RRFConstant: evalFusionRRFConstant, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_0_25", Description: "E5 当前最优 BM25 权重候选", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: evalFusionRRFConstant, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_0_1", Description: "E6 弱 BM25 补充", Weights: evalRRFWeights{Dense: 1, BM25: 0.1}, RRFConstant: evalFusionRRFConstant, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_k_5", Description: "E7 固定最优权重，更强调前排排名", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 5, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_k_10", Description: "E8 固定最优权重的当前 k 基线", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 10, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_k_30", Description: "E9 固定最优权重，中等平滑", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 30, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_k_60", Description: "E10 固定最优权重，经典 RRF 参数", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 60, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_raw", Description: "E11 Dense + 当前 ASR/OCR BM25", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 10, BM25Source: "raw", BM25FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "dense_bm25_retrieval", Description: "E12 Dense + retrieval_text BM25", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 10, BM25Source: "retrieval", BM25FieldWeights: evalBM25FieldWeights{Retrieval: 1}},
		{Name: "dense_bm25_hybrid_fields", Description: "E13 Dense + 加权字段 BM25", Weights: evalRRFWeights{Dense: 1, BM25: 0.25}, RRFConstant: 10, BM25Source: "hybrid", BM25FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 0.25, OCR: 0.25}},
	}
}

func evalRRFConstantLabel(value float64) string {
	if value <= 0 {
		return "-"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func TestRetrievalOfflineEval(t *testing.T) {
	startedAt := time.Now().UTC()
	repoRoot, err := retrievalEvalRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	// 先加载根目录 .env，评测开关因此既可写入文件，也可由命令行环境变量覆盖。
	envErr := godotenv.Load(filepath.Join(repoRoot, ".env"))
	if envErr != nil && !errors.Is(envErr, os.ErrNotExist) {
		t.Fatalf("加载项目 .env: %v", envErr)
	}
	if os.Getenv(retrievalEvalRunEnv) != "1" {
		t.Skip("将 .env 中 RUN_RETRIEVAL_EVAL 设为 1 后才执行离线检索测评")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载项目配置: %v", err)
	}
	evalDatabaseURL := strings.TrimSpace(os.Getenv(retrievalEvalDatabaseEnv))
	if err := validateEvalDatabaseIsolation(evalDatabaseURL, cfg.DatabaseURL); err != nil {
		t.Fatalf("评测数据库配置无效: %v", err)
	}

	datasetPath := retrievalEvalPath(repoRoot, retrievalEvalDatasetEnv, filepath.Join("docs", "evaldata", "retrieval_tuning_questions.jsonl"))
	resultsPath := retrievalEvalPath(repoRoot, retrievalEvalResultsEnv, filepath.Join("docs", "evaldata", "retrieval_eval_runs.jsonl"))
	questions, err := loadEvalQuestions(datasetPath)
	if err != nil {
		t.Fatalf("加载评测集: %v", err)
	}
	datasetHash, err := evalFileSHA256(datasetPath)
	if err != nil {
		t.Fatalf("计算评测集哈希: %v", err)
	}
	// 正式评测与调参使用不同冻结快照，运行记录必须计算本次实际快照的哈希。
	databaseSnapshotPath := retrievalEvalPath(repoRoot, retrievalEvalSnapshotEnv, filepath.Join("docs", "evaldata", "retrieval_tuning_database.sql"))
	databaseSnapshotHash, err := evalFileSHA256(databaseSnapshotPath)
	if err != nil {
		t.Fatalf("计算评测数据库快照哈希: %v", err)
	}

	// 评测连接只使用专用 URL，禁止缺失时回退到线上业务数据库。
	gormDB, err := db.Open(evalDatabaseURL)
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatalf("获取 PostgreSQL 连接: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("关闭 PostgreSQL 连接: %v", err)
		}
	})

	retriever, err := newEvalRetriever(gormDB, cfg.RAGBaseURL, cfg.RAGAPIKey, cfg.RAGEmbeddingModel, cfg.RAGRequestTimeout, cfg.ChatRAGMinSimilarity)
	if err != nil {
		t.Fatalf("创建测试专用检索器: %v", err)
	}
	specs := evalExperimentSpecs()
	aggregates := make([]evalAggregate, len(specs))
	questionResults := make([]evalQuestionResult, 0, len(questions))
	ctx := context.Background()
	for index, question := range questions {
		// 每题只请求一次 Embedding，全部实验共享完全相同的 Dense 结果和数据库窗口。
		channels, err := runEvalChannels(ctx, retriever, question)
		if err != nil {
			t.Fatalf("问题 %s 执行测试专用检索: %v", question.QuestionID, err)
		}
		goldGroups, err := validateGoldEvidenceGroups(channels.Segments, question.GoldEvidenceGroups)
		if err != nil {
			t.Fatalf("问题 %s 校验答案证据组: %v", question.QuestionID, err)
		}
		experimentResults := make([]evalQuestionExperimentResult, 0, len(specs))
		for experimentIndex, spec := range specs {
			bm25Hits := evalBM25HitsForExperiment(question.Question, channels, spec)
			candidates := evalFuseRetrievalChannels(channels.Segments, channels.Dense, bm25Hits, spec.Weights, spec.RRFConstant)
			metrics := measureEvidenceGroupsAt5(candidates, goldGroups)
			aggregates[experimentIndex].Add(metrics)
			experimentResults = append(experimentResults, evalQuestionExperimentResult{Name: spec.Name, Metrics: metrics, Candidates: evalCandidateResults(candidates)})
		}
		questionResults = append(questionResults, evalQuestionResult{
			QuestionID: question.QuestionID, TenantID: question.TenantID, VideoID: question.VideoID,
			VideoTitle: question.VideoTitle, Question: question.Question, Answer: question.Answer, Tags: question.Tags,
			GoldEvidenceGroups: question.GoldEvidenceGroups, Channels: evalChannelResult(channels), Experiments: experimentResults,
		})
		// E8 是最优权重在 k=10 时的第二阶段基线，进度日志固定展示该组。
		candidate := experimentResults[7].Metrics
		t.Logf("进度 %d/%d question_id=%s e8_recall=%.2f e8_rr=%.2f", index+1, len(questions), question.QuestionID, candidate.RecallAt5, candidate.RRAt5)
	}

	summaries := evalExperimentSummaries(specs, aggregates)
	run := evalRunRecord{
		SchemaVersion: evalResultSchemaVersion,
		RunID:         startedAt.Format("20060102T150405.000000000Z") + "-" + uuid.NewString(),
		StartedAt:     startedAt,
		CompletedAt:   time.Now().UTC(),
		QuestionCount: len(questions),
		Config: evalRunConfig{
			DatasetPath: evalRelativePath(repoRoot, datasetPath), DatasetSHA256: datasetHash,
			DatabaseSnapshotPath: evalRelativePath(repoRoot, databaseSnapshotPath), DatabaseSnapshotSHA256: databaseSnapshotHash,
			DatabaseIdentity: databaseIdentity(mustParseEvalDatabaseURL(evalDatabaseURL)), EmbeddingModel: cfg.RAGEmbeddingModel,
			MinSimilarity: cfg.ChatRAGMinSimilarity, ChannelTopK: evalChannelTopK, FinalTopK: evalFinalTopK,
		},
		Experiments: summaries,
		Questions:   questionResults,
	}
	// 每次成功运行都单独保存一份逐题 JSONL，便于沿用旧结果文件的检查方式。
	detailPath := evalRunDetailPath(resultsPath, run.RunID)
	if err := writeEvalQuestionResults(detailPath, run); err != nil {
		t.Fatalf("写入本次评测详细结果: %v", err)
	}
	// 只有全部问题和全部实验成功后才追加一条完整运行记录，历史运行永不覆盖。
	if err := appendEvalRun(resultsPath, run); err != nil {
		t.Fatalf("追加完整评测运行记录: %v", err)
	}
	t.Logf("questions=%d run_id=%s", len(questions), run.RunID)
	for _, summary := range summaries {
		t.Logf("%-28s Recall@5=%.4f MRR@5=%.4f delta=%+.2f/%+.2f points weights=%.2f/%.2f k=%s bm25=%s fields=%.2f/%.2f/%.2f",
			summary.Name, summary.Metrics.RecallAt5, summary.Metrics.RRAt5,
			summary.RecallDeltaVsDensePoints, summary.MRRDeltaVsDensePoints,
			summary.Weights.Dense, summary.Weights.BM25, evalRRFConstantLabel(summary.RRFConstant), evalBM25SourceLabel(summary.BM25Source),
			summary.BM25FieldWeights.Retrieval, summary.BM25FieldWeights.Transcript, summary.BM25FieldWeights.OCR)
	}
	t.Logf("完整运行记录已追加至 %s", resultsPath)
	t.Logf("本次逐题详细结果已保存至 %s", detailPath)
}

func newEvalRetriever(database *gorm.DB, baseURL, apiKey, modelName string, timeout time.Duration, minSimilarity float64) (*evalRetriever, error) {
	if database == nil {
		return nil, errors.New("evaluation retriever requires database")
	}
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(baseURL))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("RAG_BASE_URL must be an absolute HTTP(S) URL for evaluation retrieval")
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelName) == "" {
		return nil, errors.New("RAG_API_KEY and RAG_EMBEDDING_MODEL are required for evaluation retrieval")
	}
	if timeout <= 0 || minSimilarity < -1 || minSimilarity > 1 {
		return nil, errors.New("evaluation retrieval timeout or similarity threshold is invalid")
	}
	client := openai.NewClient(
		option.WithAPIKey(strings.TrimSpace(apiKey)), option.WithBaseURL(endpoint.String()),
		option.WithHTTPClient(&http.Client{Timeout: timeout}), option.WithMaxRetries(2),
	)
	return &evalRetriever{db: database, embeddingClient: client, embeddingModel: strings.TrimSpace(modelName), minSimilarity: minSimilarity}, nil
}

func runEvalChannels(ctx context.Context, retriever *evalRetriever, question evalQuestion) (evalRawChannels, error) {
	segments, err := retriever.loadSegments(ctx, question.TenantID, question.VideoID)
	if err != nil {
		return evalRawChannels{}, err
	}
	if len(segments) == 0 {
		return evalRawChannels{}, fmt.Errorf("视频 %s 没有可评测的 RAG 窗口", question.VideoID)
	}
	documents := evalBuildSegmentTextDocuments(segments)
	vector, err := retriever.embedQuery(ctx, question.Question)
	if err != nil {
		return evalRawChannels{}, err
	}
	dense, err := retriever.semanticSearch(ctx, question.TenantID, question.VideoID, vector)
	if err != nil {
		return evalRawChannels{}, err
	}
	bm25 := evalRankTextWindows(segments, evalRankBM25(question.Question, documents, 0), evalChannelTopK)
	return evalRawChannels{Segments: segments, Dense: dense, BM25: bm25}, nil
}

func evalBM25HitsForExperiment(query string, channels evalRawChannels, spec evalExperimentSpec) []evalWindowHit {
	if spec.Weights.BM25 <= 0 {
		return nil
	}
	switch spec.BM25Source {
	case "", "raw":
		return channels.BM25
	case "retrieval":
		return evalRankBM25Fields(query, channels.Segments, spec.BM25FieldWeights, false, evalChannelTopK)
	case "hybrid":
		// 混合字段组使用与独立 BM25 消融相同的查询相关 ASR/OCR 句段过滤。
		return evalRankBM25Fields(query, channels.Segments, spec.BM25FieldWeights, true, evalChannelTopK)
	default:
		panic("未知的 BM25 文本来源: " + spec.BM25Source)
	}
}

func evalBM25SourceLabel(source string) string {
	if source == "" {
		return "-"
	}
	return source
}

func (r *evalRetriever) loadSegments(ctx context.Context, tenantID, videoID string) ([]model.VideoSegment, error) {
	var segments []model.VideoSegment
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND video_id = ?", tenantID, videoID).
		Order("start_ms ASC, end_ms ASC, id ASC").Find(&segments).Error; err != nil {
		return nil, fmt.Errorf("load evaluation retrieval windows: %w", err)
	}
	return segments, nil
}

func (r *evalRetriever) embedQuery(ctx context.Context, query string) ([]float64, error) {
	response, err := r.embeddingClient.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(r.embeddingModel), Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String(query)},
		Dimensions: openai.Int(evalEmbeddingDimensions),
	}, option.WithMaxRetries(2))
	if err != nil {
		return nil, fmt.Errorf("embed evaluation retrieval query: %w", err)
	}
	if len(response.Data) != 1 || len(response.Data[0].Embedding) != evalEmbeddingDimensions {
		return nil, errors.New("evaluation query embedding response has invalid dimensions")
	}
	return response.Data[0].Embedding, nil
}

func (r *evalRetriever) semanticSearch(ctx context.Context, tenantID, videoID string, embedding []float64) ([]evalDenseHit, error) {
	vector, err := evalFormatVector(embedding)
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
		LIMIT ?`, vector, tenantID, videoID, vector, evalChannelTopK).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query evaluation semantic candidates: %w", err)
	}
	hits := make([]evalDenseHit, 0, len(rows))
	for _, row := range rows {
		if row.Similarity >= r.minSimilarity {
			hits = append(hits, evalDenseHit{SegmentID: row.SegmentID, Similarity: row.Similarity})
		}
	}
	return hits, nil
}

func evalFormatVector(vector []float64) (string, error) {
	if len(vector) != evalEmbeddingDimensions {
		return "", fmt.Errorf("vector has %d dimensions, want %d", len(vector), evalEmbeddingDimensions)
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

func evalBuildSegmentTextDocuments(segments []model.VideoSegment) []evalTextDocument {
	documents := make([]evalTextDocument, 0, len(segments)*2)
	for _, segment := range segments {
		if text := strings.TrimSpace(segment.Transcript); text != "" {
			documents = append(documents, evalTextDocument{SegmentID: segment.ID, SourceType: "asr", Text: text})
		}
		if text := strings.TrimSpace(segment.OCRText); text != "" {
			documents = append(documents, evalTextDocument{SegmentID: segment.ID, SourceType: "ocr", Text: text})
		}
	}
	return documents
}

func evalRankBM25(query string, documents []evalTextDocument, limit int) []evalRankedText {
	const bm25K1 = 1.2
	const bm25B = 0.75
	queryTokens := evalLexicalTokens(query)
	if len(queryTokens) == 0 || len(documents) == 0 {
		return nil
	}
	documentTokens := make([][]string, len(documents))
	documentFrequency := make(map[string]int)
	totalLength := 0
	for index, document := range documents {
		tokens := evalLexicalTokens(document.Text)
		documentTokens[index] = tokens
		totalLength += len(tokens)
		seen := make(map[string]struct{}, len(tokens))
		for _, token := range tokens {
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			documentFrequency[token]++
		}
	}
	averageLength := float64(totalLength) / float64(len(documents))
	if averageLength == 0 {
		return nil
	}
	queryFrequency := make(map[string]int)
	for _, token := range queryTokens {
		queryFrequency[token]++
	}
	ranked := make([]evalRankedText, 0, len(documents))
	for index, tokens := range documentTokens {
		termFrequency := make(map[string]int)
		for _, token := range tokens {
			termFrequency[token]++
		}
		var score float64
		for token, queryCount := range queryFrequency {
			frequency := termFrequency[token]
			if frequency == 0 {
				continue
			}
			df := documentFrequency[token]
			idf := math.Log(1 + (float64(len(documents)-df)+0.5)/(float64(df)+0.5))
			lengthNorm := 1 - bm25B + bm25B*float64(len(tokens))/averageLength
			score += float64(queryCount) * idf * (float64(frequency) * (bm25K1 + 1)) / (float64(frequency) + bm25K1*lengthNorm)
		}
		if score > 0 {
			ranked = append(ranked, evalRankedText{Document: documents[index], Score: score})
		}
	}
	return evalSortRankedTexts(ranked, limit)
}

func evalSortRankedTexts(ranked []evalRankedText, limit int) []evalRankedText {
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			if ranked[i].Document.SegmentID == ranked[j].Document.SegmentID {
				return ranked[i].Document.SourceType < ranked[j].Document.SourceType
			}
			return ranked[i].Document.SegmentID < ranked[j].Document.SegmentID
		}
		return ranked[i].Score > ranked[j].Score
	})
	if limit > 0 && len(ranked) > limit {
		return ranked[:limit]
	}
	return ranked
}

func evalLexicalTokens(input string) []string {
	input = strings.ToLower(input)
	var tokens []string
	var latin, han []rune
	flushLatin := func() {
		if len(latin) > 0 {
			tokens = append(tokens, string(latin))
			latin = latin[:0]
		}
	}
	flushHan := func() {
		if len(han) == 1 {
			tokens = append(tokens, string(han))
		} else {
			for index := 0; index+1 < len(han); index++ {
				token := string(han[index : index+2])
				if _, stop := evalLexicalStopTokens[token]; !stop {
					tokens = append(tokens, token)
				}
			}
		}
		han = han[:0]
	}
	for _, current := range input {
		switch {
		case unicode.Is(unicode.Han, current):
			flushLatin()
			han = append(han, current)
		case unicode.IsLetter(current) || unicode.IsDigit(current) || strings.ContainsRune("._:+#/%-", current):
			flushHan()
			latin = append(latin, current)
		default:
			flushLatin()
			flushHan()
		}
	}
	flushLatin()
	flushHan()
	return tokens
}

func evalRankTextWindows(segments []model.VideoSegment, hits []evalRankedText, limit int) []evalWindowHit {
	bestScores := make(map[string]float64)
	for _, hit := range hits {
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
	results := make([]evalWindowHit, 0, len(ranked))
	currentRank := 0
	for index, window := range ranked {
		if index == 0 || window.Score != ranked[index-1].Score {
			currentRank = index + 1
		}
		results = append(results, evalWindowHit{SegmentID: window.ID, Rank: currentRank, Score: window.Score})
	}
	return results
}

func evalFuseRetrievalChannels(segments []model.VideoSegment, dense []evalDenseHit, bm25 []evalWindowHit, weights evalRRFWeights, rrfConstant float64) []evalCandidate {
	if rrfConstant < 0 {
		panic("RRF constant 不能为负数")
	}
	candidates := make(map[string]*evalCandidate, len(segments))
	segmentByID := make(map[string]model.VideoSegment, len(segments))
	for _, segment := range segments {
		segmentByID[segment.ID] = segment
	}
	ensure := func(segmentID string) *evalCandidate {
		candidate := candidates[segmentID]
		if candidate == nil {
			candidate = &evalCandidate{Segment: segmentByID[segmentID]}
			candidates[segmentID] = candidate
		}
		return candidate
	}
	if weights.Dense > 0 {
		for index, hit := range dense {
			if _, ok := segmentByID[hit.SegmentID]; !ok {
				continue
			}
			candidate := ensure(hit.SegmentID)
			candidate.DenseRank, candidate.DenseScore = index+1, hit.Similarity
		}
	}
	if weights.BM25 > 0 {
		for _, hit := range bm25 {
			if _, ok := segmentByID[hit.SegmentID]; !ok {
				continue
			}
			candidate := ensure(hit.SegmentID)
			candidate.BM25Rank, candidate.BM25Score = hit.Rank, hit.Score
		}
	}
	result := make([]evalCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.DenseRank > 0 {
			candidate.DenseContribution = weights.Dense / (rrfConstant + float64(candidate.DenseRank))
		}
		if candidate.BM25Rank > 0 {
			candidate.BM25Contribution = weights.BM25 / (rrfConstant + float64(candidate.BM25Rank))
		}
		candidate.RRFScore = candidate.DenseContribution + candidate.BM25Contribution
		result = append(result, *candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].RRFScore == result[j].RRFScore {
			return result[i].Segment.StartMS < result[j].Segment.StartMS
		}
		return result[i].RRFScore > result[j].RRFScore
	})
	if len(result) > evalFinalTopK {
		result = result[:evalFinalTopK]
	}
	return result
}

func measureEvidenceGroupsAt5(candidates []evalCandidate, groups []map[string]struct{}) evalMetrics {
	if len(groups) == 0 {
		panic("评测问题必须至少有一个答案证据组")
	}
	hitGroups := make(map[int]struct{}, len(groups))
	firstRelevantRank := 0
	limit := min(evalFinalTopK, len(candidates))
	for candidateIndex := 0; candidateIndex < limit; candidateIndex++ {
		segmentID := candidates[candidateIndex].Segment.ID
		for groupIndex, group := range groups {
			if _, ok := group[segmentID]; !ok {
				continue
			}
			hitGroups[groupIndex] = struct{}{}
			if firstRelevantRank == 0 {
				firstRelevantRank = candidateIndex + 1
			}
			break
		}
	}
	rr := 0.0
	if firstRelevantRank > 0 {
		rr = 1 / float64(firstRelevantRank)
	}
	return evalMetrics{RecallAt5: float64(len(hitGroups)) / float64(len(groups)), RRAt5: rr}
}

func validateGoldEvidenceGroups(segments []model.VideoSegment, rawGroups [][]string) ([]map[string]struct{}, error) {
	available := make(map[string]struct{}, len(segments))
	for _, segment := range segments {
		available[segment.ID] = struct{}{}
	}
	seenGlobally := make(map[string]struct{})
	groups := make([]map[string]struct{}, 0, len(rawGroups))
	for groupIndex, rawGroup := range rawGroups {
		if len(rawGroup) == 0 {
			return nil, fmt.Errorf("gold evidence group %d 不能为空", groupIndex+1)
		}
		group := make(map[string]struct{}, len(rawGroup))
		for _, segmentID := range rawGroup {
			if _, ok := available[segmentID]; !ok {
				return nil, fmt.Errorf("gold segment %s 不属于当前租户和视频", segmentID)
			}
			if _, duplicate := seenGlobally[segmentID]; duplicate {
				return nil, fmt.Errorf("gold segment %s 在多个证据组中重复", segmentID)
			}
			seenGlobally[segmentID] = struct{}{}
			group[segmentID] = struct{}{}
		}
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil, errors.New("评测问题必须至少有一个 gold evidence group")
	}
	return groups, nil
}

func evalChannelResult(channels evalRawChannels) evalChannelResults {
	segmentByID := make(map[string]model.VideoSegment, len(channels.Segments))
	for _, segment := range channels.Segments {
		segmentByID[segment.ID] = segment
	}
	dense := make([]evalChannelCandidateResult, 0, len(channels.Dense))
	for index, hit := range channels.Dense {
		segment := segmentByID[hit.SegmentID]
		dense = append(dense, evalChannelCandidateResult{Rank: index + 1, SegmentID: hit.SegmentID, StartMS: segment.StartMS, EndMS: segment.EndMS, Score: hit.Similarity})
	}
	return evalChannelResults{Dense: dense, BM25: evalTextChannelResults(channels.BM25, segmentByID)}
}

func evalTextChannelResults(hits []evalWindowHit, segmentByID map[string]model.VideoSegment) []evalChannelCandidateResult {
	results := make([]evalChannelCandidateResult, 0, len(hits))
	for _, hit := range hits {
		segment := segmentByID[hit.SegmentID]
		results = append(results, evalChannelCandidateResult{Rank: hit.Rank, SegmentID: hit.SegmentID, StartMS: segment.StartMS, EndMS: segment.EndMS, Score: hit.Score})
	}
	return results
}

func evalCandidateResults(candidates []evalCandidate) []evalCandidateResult {
	results := make([]evalCandidateResult, 0, len(candidates))
	for index, candidate := range candidates {
		results = append(results, evalCandidateResult{
			ResultRank: index + 1, SegmentID: candidate.Segment.ID, StartMS: candidate.Segment.StartMS, EndMS: candidate.Segment.EndMS,
			DenseRank: candidate.DenseRank, DenseScore: candidate.DenseScore, BM25Rank: candidate.BM25Rank,
			BM25Score: candidate.BM25Score, DenseContribution: candidate.DenseContribution,
			BM25Contribution: candidate.BM25Contribution, RRFScore: candidate.RRFScore,
		})
	}
	return results
}

func evalExperimentSummaries(specs []evalExperimentSpec, aggregates []evalAggregate) []evalExperimentSummary {
	denseMetrics := aggregates[0].Metrics()
	summaries := make([]evalExperimentSummary, 0, len(specs))
	for index, spec := range specs {
		metrics := aggregates[index].Metrics()
		summaries = append(summaries, evalExperimentSummary{
			Name: spec.Name, Description: spec.Description, Weights: spec.Weights, RRFConstant: spec.RRFConstant,
			BM25Source: spec.BM25Source, BM25FieldWeights: spec.BM25FieldWeights, Metrics: metrics,
			RecallDeltaVsDensePoints: (metrics.RecallAt5 - denseMetrics.RecallAt5) * 100,
			MRRDeltaVsDensePoints:    (metrics.RRAt5 - denseMetrics.RRAt5) * 100,
		})
	}
	return summaries
}

func retrievalEvalRepoRoot() (string, error) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("无法定位评测测试文件")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..")), nil
}

func retrievalEvalPath(repoRoot, envName, defaultRelativePath string) string {
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		path = defaultRelativePath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(repoRoot, filepath.Clean(path))
}

func validateEvalDatabaseIsolation(evalDatabaseURL, businessDatabaseURL string) error {
	if strings.TrimSpace(evalDatabaseURL) == "" {
		return fmt.Errorf("必须设置 %s，且不能复用 DATABASE_URL", retrievalEvalDatabaseEnv)
	}
	evalURL, err := url.ParseRequestURI(evalDatabaseURL)
	if err != nil || evalURL.Hostname() == "" || strings.Trim(evalURL.Path, "/") == "" {
		return errors.New("RETRIEVAL_EVAL_DATABASE_URL 必须是包含主机和数据库名的 PostgreSQL URL")
	}
	businessURL, err := url.ParseRequestURI(strings.TrimSpace(businessDatabaseURL))
	if err != nil {
		return fmt.Errorf("解析业务 DATABASE_URL: %w", err)
	}
	if databaseIdentity(evalURL) == databaseIdentity(businessURL) {
		return errors.New("RETRIEVAL_EVAL_DATABASE_URL 与业务 DATABASE_URL 指向同一数据库")
	}
	return nil
}

func mustParseEvalDatabaseURL(rawURL string) *url.URL {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		panic(err)
	}
	return parsed
}

func databaseIdentity(databaseURL *url.URL) string {
	port := databaseURL.Port()
	if port == "" {
		port = "5432"
	}
	return strings.ToLower(databaseURL.Hostname()) + ":" + port + "/" + strings.Trim(databaseURL.Path, "/")
}

func loadEvalQuestions(path string) ([]evalQuestion, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	questions := make([]evalQuestion, 0)
	seenQuestionIDs := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var question evalQuestion
		if err := json.Unmarshal(line, &question); err != nil {
			return nil, fmt.Errorf("第 %d 行 JSON 无效: %w", lineNumber, err)
		}
		if err := validateEvalQuestion(question, seenQuestionIDs); err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", lineNumber, err)
		}
		seenQuestionIDs[question.QuestionID] = struct{}{}
		questions = append(questions, question)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取测试集: %w", err)
	}
	if len(questions) == 0 {
		return nil, errors.New("测试集不能为空")
	}
	return questions, nil
}

func validateEvalQuestion(question evalQuestion, seenQuestionIDs map[string]struct{}) error {
	if strings.TrimSpace(question.QuestionID) == "" {
		return errors.New("question_id 不能为空")
	}
	if _, duplicate := seenQuestionIDs[question.QuestionID]; duplicate {
		return fmt.Errorf("question_id %s 重复", question.QuestionID)
	}
	if _, err := uuid.Parse(question.TenantID); err != nil {
		return fmt.Errorf("tenant_id %q 不是合法 UUID", question.TenantID)
	}
	if _, err := uuid.Parse(question.VideoID); err != nil {
		return fmt.Errorf("video_id %q 不是合法 UUID", question.VideoID)
	}
	if strings.TrimSpace(question.Question) == "" {
		return errors.New("question 不能为空")
	}
	if len(question.GoldEvidenceGroups) == 0 {
		return errors.New("gold_evidence_groups 至少需要一个证据组")
	}
	seenSegmentIDs := make(map[string]struct{})
	for groupIndex, group := range question.GoldEvidenceGroups {
		if len(group) == 0 {
			return fmt.Errorf("gold_evidence_groups[%d] 不能为空", groupIndex)
		}
		for _, segmentID := range group {
			if _, err := uuid.Parse(segmentID); err != nil {
				return fmt.Errorf("gold segment id %q 不是合法 UUID", segmentID)
			}
			if _, duplicate := seenSegmentIDs[segmentID]; duplicate {
				return fmt.Errorf("gold segment id %s 跨证据组重复", segmentID)
			}
			seenSegmentIDs[segmentID] = struct{}{}
		}
	}
	return nil
}

func evalFileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func evalRelativePath(repoRoot, path string) string {
	relative, err := filepath.Rel(repoRoot, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return filepath.Clean(path)
	}
	return filepath.ToSlash(relative)
}

func appendEvalRun(path string, run evalRunRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	return file.Sync()
}

func evalRunDetailPath(historyPath, runID string) string {
	// 详细文件放在共享历史文件同级的子目录中，避免多次运行互相覆盖。
	return filepath.Join(filepath.Dir(historyPath), "retrieval_eval_results", runID+".jsonl")
}

func writeEvalQuestionResults(path string, run evalRunRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// O_EXCL 防止意外覆盖同一 run_id 已有的详细结果。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, question := range run.Questions {
		if err := encoder.Encode(question); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func TestMeasureEvidenceGroupsAt5TreatsAlternativesAsOneEvidence(t *testing.T) {
	candidates := []evalCandidate{{Segment: model.VideoSegment{ID: "alternative-a"}}, {Segment: model.VideoSegment{ID: "irrelevant"}}}
	groups := []map[string]struct{}{{"gold-a": {}, "alternative-a": {}}, {"gold-b": {}}}
	metrics := measureEvidenceGroupsAt5(candidates, groups)
	if math.Abs(metrics.RecallAt5-0.5) > 1e-9 {
		t.Fatalf("Recall@5 = %v, want 0.5", metrics.RecallAt5)
	}
	if math.Abs(metrics.RRAt5-1) > 1e-9 {
		t.Fatalf("RR@5 = %v, want 1", metrics.RRAt5)
	}
}

func TestWeightedBM25DoesNotBeatDenseFirstWithoutDenseSupport(t *testing.T) {
	segments := []model.VideoSegment{{ID: "dense", StartMS: 0}, {ID: "bm25", StartMS: 60_000}}
	dense := []evalDenseHit{{SegmentID: "dense", Similarity: 0.9}}
	bm25 := []evalWindowHit{{SegmentID: "bm25", Rank: 1, Score: 10}}
	result := evalFuseRetrievalChannels(segments, dense, bm25, evalRRFWeights{Dense: 1, BM25: 0.25}, evalFusionRRFConstant)
	if len(result) != 2 || result[0].Segment.ID != "dense" {
		t.Fatalf("weighted RRF order = %#v, want dense first", result)
	}
}

func TestEvalExperimentSpecsMatchTwoStageDesign(t *testing.T) {
	expected := []struct {
		name     string
		weights  evalRRFWeights
		constant float64
	}{
		{name: "dense_only", weights: evalRRFWeights{Dense: 1}},
		{name: "bm25_only", weights: evalRRFWeights{BM25: 1}},
		{name: "dense_bm25_equal", weights: evalRRFWeights{Dense: 1, BM25: 1}, constant: 10},
		{name: "dense_bm25_0_5", weights: evalRRFWeights{Dense: 1, BM25: 0.5}, constant: 10},
		{name: "dense_bm25_0_25", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 10},
		{name: "dense_bm25_0_1", weights: evalRRFWeights{Dense: 1, BM25: 0.1}, constant: 10},
		{name: "dense_bm25_k_5", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 5},
		{name: "dense_bm25_k_10", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 10},
		{name: "dense_bm25_k_30", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 30},
		{name: "dense_bm25_k_60", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 60},
		{name: "dense_bm25_raw", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 10},
		{name: "dense_bm25_retrieval", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 10},
		{name: "dense_bm25_hybrid_fields", weights: evalRRFWeights{Dense: 1, BM25: 0.25}, constant: 10},
	}
	specs := evalExperimentSpecs()
	if len(specs) != len(expected) {
		t.Fatalf("experiment count = %d, want %d", len(specs), len(expected))
	}
	for index, want := range expected {
		got := specs[index]
		if got.Name != want.name || got.Weights != want.weights || got.RRFConstant != want.constant {
			t.Fatalf("experiment %d = %s %+v k=%v, want %s %+v k=%v", index+1, got.Name, got.Weights, got.RRFConstant, want.name, want.weights, want.constant)
		}
	}
	if specs[10].BM25Source != "raw" || specs[11].BM25Source != "retrieval" || specs[12].BM25Source != "hybrid" {
		t.Fatalf("新增 BM25 来源实验配置错误: %q/%q/%q", specs[10].BM25Source, specs[11].BM25Source, specs[12].BM25Source)
	}
	wantHybridFields := evalBM25FieldWeights{Retrieval: 1, Transcript: 0.25, OCR: 0.25}
	if specs[12].BM25FieldWeights != wantHybridFields {
		t.Fatalf("混合字段权重 = %+v, want %+v", specs[12].BM25FieldWeights, wantHybridFields)
	}
}

func TestAppendEvalRunPreservesCompletedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrieval_eval_runs.jsonl")
	for _, runID := range []string{"run-1", "run-2"} {
		if err := appendEvalRun(path, evalRunRecord{SchemaVersion: evalResultSchemaVersion, RunID: runID}); err != nil {
			t.Fatalf("append %s: %v", runID, err)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(content), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("history lines = %d, want 2", len(lines))
	}
	for index, line := range lines {
		var run evalRunRecord
		if err := json.Unmarshal(line, &run); err != nil {
			t.Fatalf("decode history line %d: %v", index+1, err)
		}
		if run.RunID != fmt.Sprintf("run-%d", index+1) {
			t.Fatalf("history line %d run_id = %q", index+1, run.RunID)
		}
	}
}

func TestWriteEvalQuestionResultsContainsEveryQuestion(t *testing.T) {
	run := evalRunRecord{
		SchemaVersion: evalResultSchemaVersion,
		RunID:         "run-detail",
		QuestionCount: 2,
		Questions: []evalQuestionResult{
			{QuestionID: "question-1", Question: "问题一"},
			{QuestionID: "question-2", Question: "问题二"},
		},
	}
	historyPath := filepath.Join(t.TempDir(), "retrieval_eval_runs.jsonl")
	detailPath := evalRunDetailPath(historyPath, run.RunID)
	if err := writeEvalQuestionResults(detailPath, run); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(detailPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(content), []byte{'\n'})
	if len(lines) != run.QuestionCount {
		t.Fatalf("detail question count = %d, want %d", len(lines), run.QuestionCount)
	}
	var lastQuestion evalQuestionResult
	if err := json.Unmarshal(lines[1], &lastQuestion); err != nil {
		t.Fatalf("decode detail question: %v", err)
	}
	if lastQuestion.QuestionID != "question-2" {
		t.Fatalf("last detail question = %q, want question-2", lastQuestion.QuestionID)
	}
	if err := writeEvalQuestionResults(detailPath, run); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate detail write error = %v, want os.ErrExist", err)
	}
}

func TestRetrievalEvalDatasetUsesEvidenceGroups(t *testing.T) {
	repoRoot, err := retrievalEvalRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	questions, err := loadEvalQuestions(filepath.Join(repoRoot, "docs", "evaldata", "retrieval_tuning_questions.jsonl"))
	if err != nil {
		t.Fatalf("load evaluation dataset: %v", err)
	}
	if len(questions) != 120 {
		t.Fatalf("question count = %d, want 120", len(questions))
	}
	alternativeGroups := 0
	for _, question := range questions {
		for _, group := range question.GoldEvidenceGroups {
			if len(group) > 1 {
				alternativeGroups++
			}
		}
	}
	if alternativeGroups == 0 {
		t.Fatal("evaluation dataset has no equivalent answer window groups")
	}
}

func TestRetrievalEvaluationDatasetUsesEvidenceGroups(t *testing.T) {
	repoRoot, err := retrievalEvalRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	questions, err := loadEvalQuestions(filepath.Join(repoRoot, "docs", "evaldata", "retrieval_evaluation_questions.jsonl"))
	if err != nil {
		t.Fatalf("load formal evaluation dataset: %v", err)
	}
	if len(questions) != 80 {
		t.Fatalf("formal evaluation question count = %d, want 80", len(questions))
	}
	// 正式集必须同时覆盖等价答案窗口与多证据问题，避免评测只反映单窗口命中。
	alternativeGroups := 0
	multiEvidenceQuestions := 0
	for _, question := range questions {
		if len(question.GoldEvidenceGroups) > 1 {
			multiEvidenceQuestions++
		}
		for _, group := range question.GoldEvidenceGroups {
			if len(group) > 1 {
				alternativeGroups++
			}
		}
	}
	if alternativeGroups == 0 {
		t.Fatal("formal evaluation dataset has no equivalent answer window groups")
	}
	if multiEvidenceQuestions == 0 {
		t.Fatal("formal evaluation dataset has no multi-evidence questions")
	}
}

func TestValidateGoldEvidenceGroupsRejectsWrongVideoWindow(t *testing.T) {
	_, err := validateGoldEvidenceGroups([]model.VideoSegment{{ID: "available"}}, [][]string{{"missing"}})
	if err == nil {
		t.Fatal("expected an error for a gold segment outside the loaded video")
	}
}

func TestValidateEvalDatabaseIsolationRejectsBusinessDatabase(t *testing.T) {
	businessURL := "postgres://readergo:business@localhost:5432/readergo?sslmode=disable"
	evalURL := "postgres://readergo:different-password@localhost/readergo?sslmode=require"
	if err := validateEvalDatabaseIsolation(evalURL, businessURL); err == nil {
		t.Fatal("expected the evaluation database check to reject the business database")
	}
	if err := validateEvalDatabaseIsolation("postgres://readergo:readergo@localhost:5433/readergo_eval", businessURL); err != nil {
		t.Fatalf("dedicated evaluation database was rejected: %v", err)
	}
}
