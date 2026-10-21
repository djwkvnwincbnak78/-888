package rerank

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Document is one scored candidate.
type Document struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
	Text  string  `json:"document"`
}

// BM25 is a deterministic lexical reranker used when no upstream rerank API is
// configured. It is a real ranking function (Okapi BM25), not a placeholder:
// idf weights rare query terms, tf saturates, and document length is normalised.
type BM25 struct {
	K1 float64
	B  float64
}

// New builds a BM25 scorer with the standard k1=1.2, b=0.75.
func New() *BM25 { return &BM25{K1: 1.2, B: 0.75} }

// Tokenize lowercases and splits on non-alphanumeric runes. CJK runs are kept
// as single tokens (no external segmenter).
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Rerank scores docs against query and returns the top N (N<=0 means all),
// ordered by descending score; ties keep the original document index order.
func (b *BM25) Rerank(query string, docs []string, topN int) []Document {
	q := Tokenize(query)
	if len(docs) == 0 || len(q) == 0 {
		out := make([]Document, len(docs))
		for i, d := range docs {
			out[i] = Document{Index: i, Text: d}
		}
		return truncate(out, topN)
	}

	tokens := make([][]string, len(docs))
	df := make(map[string]int)
	totalLen := 0
	for i, d := range docs {
		tokens[i] = Tokenize(d)
		totalLen += len(tokens[i])
		seen := make(map[string]struct{}, len(tokens[i]))
		for _, t := range tokens[i] {
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			df[t]++
		}
	}
	avgLen := float64(totalLen) / float64(len(docs))
	if avgLen == 0 {
		avgLen = 1
	}

	// Unique query terms with their query-side frequency.
	qf := make(map[string]int)
	for _, t := range q {
		qf[t]++
	}

	n := float64(len(docs))
	out := make([]Document, len(docs))
	for i, doc := range docs {
		tf := make(map[string]int, len(tokens[i]))
		for _, t := range tokens[i] {
			tf[t]++
		}
		dl := float64(len(tokens[i]))
		var score float64
		for term, freq := range qf {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[term])+0.5)/(float64(df[term])+0.5))
			denom := f + b.K1*(1-b.B+b.B*dl/avgLen)
			score += float64(freq) * idf * (f * (b.K1 + 1)) / denom
		}
		out[i] = Document{Index: i, Score: score, Text: doc}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Index < out[j].Index
		}
		return out[i].Score > out[j].Score
	})
	return truncate(out, topN)
}

func truncate(in []Document, topN int) []Document {
	if topN > 0 && topN < len(in) {
		return in[:topN]
	}
	return in
}
