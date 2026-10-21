// Package compress performs deterministic, semantics-preserving prompt
// compression: whitespace and punctuation only. It never rewrites, reorders or
// drops words, and it never touches fenced code blocks.
package compress

import (
	"regexp"
	"strings"
)

// Stats reports what was saved.
type Stats struct {
	OriginalChars   int
	CompressedChars int
	TokensSaved     int
}

// Text compresses prose while leaving fenced code blocks byte-identical.
func Text(s string) (string, Stats) {
	orig := len(s)
	var b strings.Builder
	b.Grow(len(s))

	// Split on ``` fences; even segments are prose, odd segments are code.
	parts := strings.Split(s, "```")
	for i, part := range parts {
		if i%2 == 1 {
			b.WriteString("```")
			b.WriteString(part)
			continue
		}
		if i > 0 {
			// closing fence of the previous code block
			b.WriteString("```")
		}
		b.WriteString(compressProse(part))
	}

	out := b.String()
	stats := Stats{OriginalChars: orig, CompressedChars: len(out)}
	if saved := orig - len(out); saved > 0 {
		stats.TokensSaved = (saved + 3) / 4
	}
	return out, stats
}

var (
	multiSpace       = regexp.MustCompile(`[ \t]{2,}`)
	multiNewline     = regexp.MustCompile(`\n{3,}`)
	spaceBeforePunct = regexp.MustCompile(`\s+([,.!?;:])`)
	bangRun          = regexp.MustCompile(`[!?]{2,}`)
	dotRun           = regexp.MustCompile(`\.{4,}`)
	separatorRun     = regexp.MustCompile(`[,;:]{2,}`)
)

func compressProse(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	s = multiSpace.ReplaceAllString(s, " ")
	s = multiNewline.ReplaceAllString(s, "\n\n")
	s = spaceBeforePunct.ReplaceAllString(s, "$1")
	s = bangRun.ReplaceAllStringFunc(s, func(run string) string {
		if run[0] == '!' {
			return "!"
		}
		return "?"
	})
	s = dotRun.ReplaceAllString(s, "...")
	s = separatorRun.ReplaceAllStringFunc(s, func(run string) string { return run[:1] })
	// Trim trailing spaces on each line without touching indentation semantics
	// of prose (code is handled separately).
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}
