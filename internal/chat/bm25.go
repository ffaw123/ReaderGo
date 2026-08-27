package chat

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"readergo/internal/model"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// bm25FieldWeights 固定生产检索中三个窗口字段的权重。
type bm25FieldWeights struct {
	RetrievalText float64
	Transcript    float64
	OCRText       float64
}

var productionBM25FieldWeights = bm25FieldWeights{RetrievalText: 1, Transcript: 0.25, OCRText: 0.25}

// bm25WindowHit 表示一个窗口在 BM25F 通道中的唯一命中及稳定排名。
type bm25WindowHit struct {
	SegmentID string
	Rank      int
	Score     float64
}

type bm25WindowFields struct {
	retrievalText []string
	transcript    []string
	ocrText       []string
}

var lexicalStopTokens = map[string]struct{}{
	"这个": {}, "那个": {}, "视频": {}, "里面": {}, "作者": {}, "什么": {}, "怎么": {},
	"为什么": {}, "是否": {}, "有没有": {}, "提到": {}, "一下": {}, "请问": {}, "关于": {},
}

// rankBM25Windows 以一个 video_segment 为一篇文档，对三个字段执行一次 BM25F 评分。
func rankBM25Windows(query string, segments []model.VideoSegment, limit int) []bm25WindowHit {
	queryTokens := lexicalTokens(query)
	if len(queryTokens) == 0 || len(segments) == 0 {
		return nil
	}

	documents := make([]bm25WindowFields, len(segments))
	documentFrequency := make(map[string]int)
	var retrievalTextTotal, transcriptTotal, ocrTextTotal int
	for index, segment := range segments {
		document := bm25WindowFields{
			retrievalText: lexicalTokens(segment.RetrievalText),
			transcript:    lexicalTokens(filterRawTextByQuery(segment.Transcript, queryTokens)),
			ocrText:       lexicalTokens(filterRawTextByQuery(segment.OCRText, queryTokens)),
		}
		documents[index] = document
		retrievalTextTotal += len(document.retrievalText)
		transcriptTotal += len(document.transcript)
		ocrTextTotal += len(document.ocrText)

		// 文档频率按窗口去重，同一词同时出现在三个字段中也只计数一次。
		seen := make(map[string]struct{})
		for _, field := range [][]string{document.retrievalText, document.transcript, document.ocrText} {
			for _, token := range field {
				seen[token] = struct{}{}
			}
		}
		for token := range seen {
			documentFrequency[token]++
		}
	}

	averageLength := func(total int) float64 {
		if total == 0 {
			return 1
		}
		return float64(total) / float64(len(segments))
	}
	retrievalTextAverage := averageLength(retrievalTextTotal)
	transcriptAverage := averageLength(transcriptTotal)
	ocrTextAverage := averageLength(ocrTextTotal)
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
			// 三个字段先各自做长度归一化，再按固定权重合并词频。
			weightedTF := bm25NormalizedTF(token, document.retrievalText, productionBM25FieldWeights.RetrievalText, retrievalTextAverage) +
				bm25NormalizedTF(token, document.transcript, productionBM25FieldWeights.Transcript, transcriptAverage) +
				bm25NormalizedTF(token, document.ocrText, productionBM25FieldWeights.OCRText, ocrTextAverage)
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
			if ranked[i].startMS == ranked[j].startMS {
				return ranked[i].segmentID < ranked[j].segmentID
			}
			return ranked[i].startMS < ranked[j].startMS
		}
		return ranked[i].score > ranked[j].score
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}

	hits := make([]bm25WindowHit, 0, len(ranked))
	currentRank := 0
	for index, window := range ranked {
		if index == 0 || window.score != ranked[index-1].score {
			currentRank = index + 1
		}
		hits = append(hits, bm25WindowHit{SegmentID: window.segmentID, Rank: currentRank, Score: window.score})
	}
	return hits
}

// filterRawTextByQuery 只保留至少包含一个查询 Token 的 ASR/OCR 句段。
func filterRawTextByQuery(text string, queryTokens []string) string {
	if len(queryTokens) == 0 || strings.TrimSpace(text) == "" {
		return ""
	}
	querySet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		querySet[token] = struct{}{}
	}
	chunks := strings.FieldsFunc(text, func(current rune) bool {
		return strings.ContainsRune("。！？!?；;，,\n\r", current)
	})
	selected := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		for _, token := range lexicalTokens(chunk) {
			if _, ok := querySet[token]; ok {
				selected = append(selected, chunk)
				break
			}
		}
	}
	return strings.Join(selected, " ")
}

func bm25NormalizedTF(token string, tokens []string, weight, averageLength float64) float64 {
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
	lengthNorm := 1 - bm25B + bm25B*float64(len(tokens))/averageLength
	return weight * float64(frequency) / lengthNorm
}

func lexicalTokens(input string) []string {
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
			// 中文使用二元字切分，不依赖外部分词服务，同时保留专有词的局部匹配能力。
			for index := 0; index+1 < len(han); index++ {
				token := string(han[index : index+2])
				if _, stop := lexicalStopTokens[token]; !stop {
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
