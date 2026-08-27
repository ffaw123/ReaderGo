package chat

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"readergo/internal/config"
	"readergo/internal/db"
	"readergo/internal/model"

	"github.com/joho/godotenv"
)

const retrievalBM25EvalRunEnv = "RUN_RETRIEVAL_BM25_EVAL"

type evalBM25FieldWeights struct {
	Retrieval  float64 `json:"retrieval_text"`
	Transcript float64 `json:"transcript"`
	OCR        float64 `json:"ocr_text"`
}

type evalBM25SourceExperiment struct {
	Name         string
	Source       string
	FieldWeights evalBM25FieldWeights
}

func TestRetrievalBM25SourceEval(t *testing.T) {
	repoRoot, err := retrievalEvalRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := godotenv.Load(filepath.Join(repoRoot, ".env")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("加载项目 .env: %v", err)
	}
	if os.Getenv(retrievalBM25EvalRunEnv) != "1" {
		t.Skip("仅通过 eval_bm25_start.bat 执行 BM25 文本来源消融测试")
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
	questions, err := loadEvalQuestions(datasetPath)
	if err != nil {
		t.Fatalf("加载调参问题集: %v", err)
	}

	gormDB, err := db.Open(evalDatabaseURL)
	if err != nil {
		t.Fatalf("连接评测 PostgreSQL: %v", err)
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

	loader := &evalRetriever{db: gormDB}
	specs := evalBM25SourceExperimentSpecs()
	aggregates := make([]evalAggregate, len(specs))
	ctx := context.Background()
	for _, question := range questions {
		// 三个实验只读取同一批数据库窗口，不请求查询向量，也不调用业务检索入口。
		segments, err := loader.loadSegments(ctx, question.TenantID, question.VideoID)
		if err != nil {
			t.Fatalf("问题 %s 读取评测窗口: %v", question.QuestionID, err)
		}
		if len(segments) == 0 {
			t.Fatalf("问题 %s 对应视频没有可评测窗口", question.QuestionID)
		}
		goldGroups, err := validateGoldEvidenceGroups(segments, question.GoldEvidenceGroups)
		if err != nil {
			t.Fatalf("问题 %s 校验答案证据组: %v", question.QuestionID, err)
		}

		rawHits := evalRankTextWindows(segments, evalRankBM25(question.Question, evalBuildSegmentTextDocuments(segments), 0), evalChannelTopK)
		retrievalHits := evalRankBM25Fields(question.Question, segments, evalBM25FieldWeights{Retrieval: 1}, false, evalChannelTopK)
		for index, spec := range specs {
			var bm25Hits []evalWindowHit
			switch spec.Source {
			case "raw":
				bm25Hits = rawHits
			case "retrieval":
				bm25Hits = retrievalHits
			case "hybrid":
				// 所有混合组使用相同的原文过滤逻辑，只改变三个字段的权重。
				bm25Hits = evalRankBM25Fields(question.Question, segments, spec.FieldWeights, true, evalChannelTopK)
			default:
				t.Fatalf("实验 %s 使用了未知 BM25 文本来源 %q", spec.Name, spec.Source)
			}
			candidates := evalFuseRetrievalChannels(segments, nil, bm25Hits, evalRRFWeights{BM25: 1}, 0)
			aggregates[index].Add(measureEvidenceGroupsAt5(candidates, goldGroups))
		}
	}

	rawMetrics := aggregates[0].Metrics()
	t.Logf("BM25 文本来源消融完成：questions=%d；以下为最终汇总，本测试未写入任何结果文件", len(questions))
	for index, spec := range specs {
		metrics := aggregates[index].Metrics()
		t.Logf("%-20s Recall@5=%.4f MRR@5=%.4f delta_vs_raw=%+.2f/%+.2f points fields=%.2f/%.2f/%.2f",
			spec.Name, metrics.RecallAt5, metrics.RRAt5,
			(metrics.RecallAt5-rawMetrics.RecallAt5)*100, (metrics.RRAt5-rawMetrics.RRAt5)*100,
			spec.FieldWeights.Retrieval, spec.FieldWeights.Transcript, spec.FieldWeights.OCR)
	}
}

func evalBM25SourceExperimentSpecs() []evalBM25SourceExperiment {
	return []evalBM25SourceExperiment{
		{Name: "bm25_raw", Source: "raw", FieldWeights: evalBM25FieldWeights{Transcript: 1, OCR: 1}},
		{Name: "bm25_retrieval", Source: "retrieval", FieldWeights: evalBM25FieldWeights{Retrieval: 1}},
		{Name: "bm25_hybrid_fields", Source: "hybrid", FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 1, OCR: 1}},
		{Name: "bm25_hybrid_0_5_0_5", Source: "hybrid", FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 0.50, OCR: 0.50}},
		{Name: "bm25_hybrid_0_25_0_25", Source: "hybrid", FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 0.25, OCR: 0.25}},
		{Name: "bm25_hybrid_0_25_0_1", Source: "hybrid", FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 0.25, OCR: 0.10}},
		{Name: "bm25_hybrid_0_1_0_1", Source: "hybrid", FieldWeights: evalBM25FieldWeights{Retrieval: 1, Transcript: 0.10, OCR: 0.10}},
	}
}

// evalRankBM25Fields 以数据库窗口为文档做 BM25F，避免 ASR 和 OCR 对同一窗口重复占用候选名额。
func evalRankBM25Fields(query string, segments []model.VideoSegment, weights evalBM25FieldWeights, filterRawFields bool, limit int) []evalWindowHit {
	const bm25K1 = 1.2
	const bm25B = 0.75
	queryTokens := evalLexicalTokens(query)
	if len(queryTokens) == 0 || len(segments) == 0 {
		return nil
	}

	type fieldTokens struct {
		retrieval  []string
		transcript []string
		ocr        []string
	}
	documents := make([]fieldTokens, len(segments))
	documentFrequency := make(map[string]int)
	var retrievalTotal, transcriptTotal, ocrTotal int
	for index, segment := range segments {
		transcript := segment.Transcript
		ocrText := segment.OCRText
		if filterRawFields {
			transcript = evalFilterRawTextByQuery(transcript, queryTokens)
			ocrText = evalFilterRawTextByQuery(ocrText, queryTokens)
		}
		document := fieldTokens{
			retrieval:  evalLexicalTokens(segment.RetrievalText),
			transcript: evalLexicalTokens(transcript),
			ocr:        evalLexicalTokens(ocrText),
		}
		documents[index] = document
		retrievalTotal += len(document.retrieval)
		transcriptTotal += len(document.transcript)
		ocrTotal += len(document.ocr)
		seen := make(map[string]struct{})
		for _, field := range []struct {
			weight float64
			tokens []string
		}{{weights.Retrieval, document.retrieval}, {weights.Transcript, document.transcript}, {weights.OCR, document.ocr}} {
			if field.weight <= 0 {
				continue
			}
			for _, token := range field.tokens {
				seen[token] = struct{}{}
			}
		}
		for token := range seen {
			documentFrequency[token]++
		}
	}

	average := func(total int) float64 {
		if total == 0 {
			return 1
		}
		return float64(total) / float64(len(segments))
	}
	retrievalAverage := average(retrievalTotal)
	transcriptAverage := average(transcriptTotal)
	ocrAverage := average(ocrTotal)
	queryFrequency := make(map[string]int)
	for _, token := range queryTokens {
		queryFrequency[token]++
	}

	type scoredWindow struct {
		segmentID string
		startMS   int64
		score     float64
	}
	ranked := make([]scoredWindow, 0, len(segments))
	for index, document := range documents {
		var score float64
		for token, queryCount := range queryFrequency {
			// 各字段先做长度归一化，再按字段权重合并词频。
			weightedTF := evalBM25NormalizedTF(token, document.retrieval, weights.Retrieval, retrievalAverage, bm25B) +
				evalBM25NormalizedTF(token, document.transcript, weights.Transcript, transcriptAverage, bm25B) +
				evalBM25NormalizedTF(token, document.ocr, weights.OCR, ocrAverage, bm25B)
			if weightedTF == 0 {
				continue
			}
			df := documentFrequency[token]
			idf := math.Log(1 + (float64(len(segments)-df)+0.5)/(float64(df)+0.5))
			score += float64(queryCount) * idf * ((bm25K1 + 1) * weightedTF) / (bm25K1 + weightedTF)
		}
		if score > 0 {
			ranked = append(ranked, scoredWindow{segmentID: segments[index].ID, startMS: segments[index].StartMS, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].startMS < ranked[j].startMS
		}
		return ranked[i].score > ranked[j].score
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	results := make([]evalWindowHit, 0, len(ranked))
	currentRank := 0
	for index, window := range ranked {
		if index == 0 || window.score != ranked[index-1].score {
			currentRank = index + 1
		}
		results = append(results, evalWindowHit{SegmentID: window.segmentID, Rank: currentRank, Score: window.score})
	}
	return results
}

// evalFilterRawTextByQuery 只保留含查询词的句段，降低长 ASR 和 OCR 原文带来的长度与无关词噪声。
func evalFilterRawTextByQuery(text string, queryTokens []string) string {
	querySet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		querySet[token] = struct{}{}
	}
	chunks := strings.FieldsFunc(text, func(current rune) bool {
		return strings.ContainsRune("。！？!?；;，,\n\r", current)
	})
	selected := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		for _, token := range evalLexicalTokens(chunk) {
			if _, ok := querySet[token]; ok {
				selected = append(selected, chunk)
				break
			}
		}
	}
	return strings.Join(selected, " ")
}

func evalBM25NormalizedTF(token string, tokens []string, weight, averageLength, b float64) float64 {
	if weight <= 0 || len(tokens) == 0 {
		return 0
	}
	frequency := 0
	for _, current := range tokens {
		if current == token {
			frequency++
		}
	}
	if frequency == 0 {
		return 0
	}
	lengthNorm := 1 - b + b*float64(len(tokens))/averageLength
	return weight * float64(frequency) / lengthNorm
}

func TestEvalRankBM25FieldsUsesConfiguredSources(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "retrieval-hit", StartMS: 0, RetrievalText: "向量数据库索引", Transcript: "无关内容"},
		{ID: "transcript-hit", StartMS: 60_000, RetrievalText: "其他主题", Transcript: "向量数据库索引"},
	}
	retrievalOnly := evalRankBM25Fields("向量数据库", segments, evalBM25FieldWeights{Retrieval: 1}, false, 8)
	if len(retrievalOnly) != 1 || retrievalOnly[0].SegmentID != "retrieval-hit" {
		t.Fatalf("仅 retrieval_text 时排名不符合预期: %+v", retrievalOnly)
	}
	hybrid := evalRankBM25Fields("向量数据库", segments, evalBM25FieldWeights{Retrieval: 1, Transcript: 1}, true, 8)
	if len(hybrid) != 2 || hybrid[0].SegmentID != "retrieval-hit" {
		t.Fatalf("混合字段检索结果不符合预期: %+v", hybrid)
	}
}

func TestProductionRetrievalMatchesIndependentEvaluationSpecification(t *testing.T) {
	segments := []model.VideoSegment{
		{ID: "s1", StartMS: 0, RetrievalText: "向量数据库 索引", Transcript: "开场闲聊。向量数据库用于检索。", OCRText: "无关标题"},
		{ID: "s2", StartMS: 60_000, RetrievalText: "数据库部署", Transcript: "无关内容", OCRText: "向量数据库；页脚噪声"},
		{ID: "s3", StartMS: 120_000, RetrievalText: "其他主题", Transcript: "向量数据库，向量数据库", OCRText: "向量数据库"},
		{ID: "s4", StartMS: 180_000, RetrievalText: "完全无关", Transcript: "没有命中", OCRText: ""},
	}
	query := "向量数据库"
	productionBM25 := rankBM25Windows(query, segments, retrievalChannelTopK)
	evaluationBM25 := evalRankBM25Fields(query, segments, evalBM25FieldWeights{Retrieval: 1, Transcript: 0.25, OCR: 0.25}, true, evalChannelTopK)
	if len(productionBM25) != len(evaluationBM25) {
		t.Fatalf("BM25 hit counts differ: production=%#v evaluation=%#v", productionBM25, evaluationBM25)
	}
	for index := range productionBM25 {
		got, want := productionBM25[index], evaluationBM25[index]
		if got.SegmentID != want.SegmentID || got.Rank != want.Rank || math.Abs(got.Score-want.Score) > 1e-12 {
			t.Fatalf("BM25 hit %d differs: production=%#v evaluation=%#v", index, got, want)
		}
	}

	dense := []semanticHit{{SegmentID: "s4", Similarity: 0.95}, {SegmentID: "s2", Similarity: 0.90}, {SegmentID: "s1", Similarity: 0.85}}
	production := fuseRetrievalChannels(segments, dense, productionBM25)
	evalDense := make([]evalDenseHit, len(dense))
	for index, hit := range dense {
		evalDense[index] = evalDenseHit{SegmentID: hit.SegmentID, Similarity: hit.Similarity}
	}
	evaluation := evalFuseRetrievalChannels(segments, evalDense, evaluationBM25, evalRRFWeights{Dense: 1, BM25: 0.25}, 10)
	if len(production) != len(evaluation) {
		t.Fatalf("fusion counts differ: production=%#v evaluation=%#v", production, evaluation)
	}
	for index := range production {
		got, want := production[index], evaluation[index]
		if got.Segment.ID != want.Segment.ID || got.DenseRank != want.DenseRank || got.BM25Rank != want.BM25Rank || math.Abs(got.RRFScore-want.RRFScore) > 1e-12 {
			t.Fatalf("fusion candidate %d differs: production=%#v evaluation=%#v", index, got, want)
		}
	}
}
