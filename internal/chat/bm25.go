package chat

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

type sourceDocument struct {
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	Text       string `json:"text"`
}

type rankedSource struct {
	Document sourceDocument
	Score    float64
}

var lexicalStopTokens = map[string]struct{}{
	"这个": {}, "那个": {}, "视频": {}, "里面": {}, "作者": {}, "什么": {}, "怎么": {},
	"为什么": {}, "是否": {}, "有没有": {}, "提到": {}, "一下": {}, "请问": {}, "关于": {},
}

func rankBM25(query string, documents []sourceDocument, limit int) []rankedSource {
	queryTokens := lexicalTokens(query)
	if len(queryTokens) == 0 || len(documents) == 0 {
		return nil
	}
	documentTokens := make([][]string, len(documents))
	documentFrequency := make(map[string]int)
	totalLength := 0
	for index, document := range documents {
		tokens := lexicalTokens(document.Text)
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

	ranked := make([]rankedSource, 0, len(documents))
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
			ranked = append(ranked, rankedSource{Document: documents[index], Score: score})
		}
	}
	return sortRankedSources(ranked, limit)
}

func rankExact(query string, documents []sourceDocument, limit int) []rankedSource {
	terms := exactTerms(query)
	if len(terms) == 0 {
		return nil
	}
	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	ranked := make([]rankedSource, 0, len(documents))
	for _, document := range documents {
		text := strings.ToLower(document.Text)
		var score float64
		if len([]rune(normalizedQuery)) >= 2 && strings.Contains(text, normalizedQuery) {
			score += 20
		}
		for _, term := range terms {
			if !strings.Contains(text, term) {
				continue
			}
			weight := 1.0
			if isHighValueExactTerm(term) {
				weight = 5
			}
			// 多次出现仍只做温和增益，避免长 ASR 因重复口头语垄断精确召回。
			occurrences := strings.Count(text, term)
			score += weight * (1 + math.Log(float64(occurrences)))
		}
		if score > 0 {
			ranked = append(ranked, rankedSource{Document: document, Score: score})
		}
	}
	return sortRankedSources(ranked, limit)
}

func sortRankedSources(ranked []rankedSource, limit int) []rankedSource {
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			if ranked[i].Document.StartMS == ranked[j].Document.StartMS {
				return ranked[i].Document.SourceID < ranked[j].Document.SourceID
			}
			return ranked[i].Document.StartMS < ranked[j].Document.StartMS
		}
		return ranked[i].Score > ranked[j].Score
	})
	if limit > 0 && len(ranked) > limit {
		return ranked[:limit]
	}
	return ranked
}

func exactTerms(query string) []string {
	all := lexicalTokens(query)
	seen := make(map[string]struct{}, len(all))
	terms := make([]string, 0, len(all))
	for _, token := range all {
		if _, stop := lexicalStopTokens[token]; stop {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
	}
	return terms
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

func isHighValueExactTerm(term string) bool {
	for _, current := range term {
		if unicode.IsDigit(current) || (current <= unicode.MaxASCII && (unicode.IsLetter(current) || strings.ContainsRune("._:+#/%-", current))) {
			return true
		}
	}
	return false
}
