package chatdiagnose

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

const (
	bm25K1                   = 1.2
	bm25B                    = 0.75
	reciprocalRankConstant   = 60.0
	lexicalCandidateMinimum  = 100
	lexicalCandidateMaximum  = 500
	lexicalSearchTermMaximum = 16
)

var bm25StopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "for": {}, "in": {}, "is": {}, "of": {}, "on": {},
	"or": {}, "the": {}, "to": {}, "with": {},
}

type rankedPattern struct {
	pattern chatdiagnosemodel.IncidentPattern
	// relevance is a bounded source score and is safe to threshold.
	relevance float64
	// score is the RRF fusion rank score and is only used for ordering.
	score float64
}

func lexicalCandidateLimit(topK int) int {
	if topK <= 0 {
		topK = 5
	}
	limit := topK * 20
	if limit < lexicalCandidateMinimum {
		limit = lexicalCandidateMinimum
	}
	if limit > lexicalCandidateMaximum {
		limit = lexicalCandidateMaximum
	}
	return limit
}

func tokenizeBM25(text string) []string {
	var tokens []string
	var asciiBuilder strings.Builder
	var cjkRun []rune
	flushCJK := func() {
		if len(cjkRun) == 0 {
			return
		}
		if len(cjkRun) == 1 {
			tokens = append(tokens, string(cjkRun))
		} else {
			for index := 0; index+1 < len(cjkRun); index++ {
				tokens = append(tokens, string(cjkRun[index:index+2]))
			}
		}
		cjkRun = nil
	}
	for _, char := range strings.ToLower(text) {
		switch {
		case char >= 0x4e00 && char <= 0x9fff:
			if asciiBuilder.Len() > 0 {
				tokens = append(tokens, asciiBuilder.String())
				asciiBuilder.Reset()
			}
			cjkRun = append(cjkRun, char)
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			flushCJK()
			asciiBuilder.WriteRune(char)
		default:
			flushCJK()
			if asciiBuilder.Len() > 0 {
				tokens = append(tokens, asciiBuilder.String())
				asciiBuilder.Reset()
			}
		}
	}
	flushCJK()
	if asciiBuilder.Len() > 0 {
		tokens = append(tokens, asciiBuilder.String())
	}
	return tokens
}

func lexicalSearchTerms(query string) []string {
	terms := make([]string, 0, lexicalSearchTermMaximum)
	seen := make(map[string]struct{})
	for _, token := range tokenizeBM25(query) {
		if _, stopped := bm25StopWords[token]; stopped {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
		if len(terms) == lexicalSearchTermMaximum {
			break
		}
	}
	return terms
}

func patternDocument(pattern chatdiagnosemodel.IncidentPattern) string {
	return strings.Join([]string{
		pattern.ResourceType,
		pattern.Symptom,
		pattern.RootCauseObject,
		pattern.Signature,
		pattern.Severity,
	}, " ")
}

func rankBM25(query string, patterns []chatdiagnosemodel.IncidentPattern, topK int) []chatdiagnosemodel.IncidentPattern {
	if query == "" || len(patterns) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}

	queryTokens := lexicalSearchTerms(query)
	if len(queryTokens) == 0 {
		return nil
	}
	queryTokenSet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		queryTokenSet[token] = struct{}{}
	}

	documents := make([]map[string]int, len(patterns))
	documentLength := make([]int, len(patterns))
	documentFrequency := make(map[string]int)
	for patternIndex, pattern := range patterns {
		terms := make(map[string]int)
		tokens := tokenizeBM25(patternDocument(pattern))
		for _, token := range tokens {
			terms[token]++
		}
		documents[patternIndex] = terms
		documentLength[patternIndex] = len(tokens)
	}
	for _, token := range queryTokens {
		for _, terms := range documents {
			if _, ok := terms[token]; ok {
				documentFrequency[token]++
			}
		}
	}

	totalLength := 0
	for _, length := range documentLength {
		totalLength += length
	}
	averageLength := float64(totalLength) / float64(len(patterns))
	if averageLength == 0 {
		averageLength = 1
	}

	scored := make([]rankedPattern, 0, len(patterns))
	for patternIndex, pattern := range patterns {
		score := 0.0
		matchedTokens := 0
		for _, token := range queryTokens {
			termFrequency := documents[patternIndex][token]
			if termFrequency == 0 {
				continue
			}
			matchedTokens++
			frequency := float64(termFrequency)
			contains := float64(documentFrequency[token])
			idf := 1.0 + (float64(len(patterns))-contains+0.5)/(contains+0.5)
			lengthNorm := 1.0 - bm25B + bm25B*float64(documentLength[patternIndex])/averageLength
			score += idf * frequency * (bm25K1 + 1.0) / (frequency + bm25K1*lengthNorm)
		}
		if score > 0 {
			pattern.Relevance = float64(matchedTokens) / float64(len(queryTokenSet))
			scored = append(scored, rankedPattern{
				pattern:   pattern,
				relevance: pattern.Relevance,
				score:     score,
			})
		}
	}
	sort.SliceStable(scored, func(left, right int) bool {
		if scored[left].score == scored[right].score {
			return scored[left].pattern.ID < scored[right].pattern.ID
		}
		return scored[left].score > scored[right].score
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}

	result := make([]chatdiagnosemodel.IncidentPattern, len(scored))
	for index, ranked := range scored {
		result[index] = ranked.pattern
	}
	return result
}

func patternIdentity(pattern chatdiagnosemodel.IncidentPattern) string {
	if pattern.ID > 0 {
		return fmt.Sprintf("id:%d", pattern.ID)
	}
	return fmt.Sprintf(
		"fallback:%s:%s:%s:%s:%s",
		pattern.TenantID,
		pattern.ResourceType,
		pattern.Symptom,
		pattern.RootCauseObject,
		pattern.Signature,
	)
}

func fusePatternRanks(vectorPatterns, lexicalPatterns []chatdiagnosemodel.IncidentPattern) []rankedPattern {
	if len(vectorPatterns) == 0 && len(lexicalPatterns) == 0 {
		return nil
	}

	scores := make(map[string]float64)
	patterns := make(map[string]chatdiagnosemodel.IncidentPattern)
	order := make([]string, 0)
	addRanks := func(ranked []chatdiagnosemodel.IncidentPattern) {
		for index, pattern := range ranked {
			identity := patternIdentity(pattern)
			if _, exists := patterns[identity]; !exists {
				patterns[identity] = pattern
				order = append(order, identity)
			}
			if pattern.Relevance > patterns[identity].Relevance {
				stored := patterns[identity]
				stored.Relevance = pattern.Relevance
				patterns[identity] = stored
			}
			scores[identity] += 1.0 / (reciprocalRankConstant + float64(index+1))
		}
	}
	addRanks(vectorPatterns)
	addRanks(lexicalPatterns)

	fused := make([]rankedPattern, 0, len(order))
	for _, identity := range order {
		fused = append(fused, rankedPattern{
			pattern:   patterns[identity],
			relevance: patterns[identity].Relevance,
			score:     scores[identity],
		})
	}
	sort.SliceStable(fused, func(left, right int) bool {
		return fused[left].score > fused[right].score
	})
	return fused
}
