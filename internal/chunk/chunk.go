// Package chunk splits long text into overlapping segments suitable for embedding.
package chunk

import (
	"regexp"
	"strings"
)

const (
	chunkSize = 2000 // ~500 tokens
	overlap   = 200  // ~50 tokens
)

// Chunk is a contiguous piece of text plus its position in the source document.
type Chunk struct {
	Index int
	Text  string
}

var whitespace = regexp.MustCompile(`\s+`)

// Split breaks text into overlapping ~2000-character chunks, preferring sentence boundaries.
func Split(text string) []Chunk {
	cleaned := strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
	if cleaned == "" {
		return nil
	}

	var chunks []Chunk
	start, idx := 0, 0

	for start < len(cleaned) {
		end := start + chunkSize
		if end >= len(cleaned) {
			end = len(cleaned)
		} else {
			// Try to break at a sentence boundary within the last 20% of the chunk.
			searchFrom := start + int(float64(chunkSize)*0.8)
			boundary := strings.LastIndex(cleaned[:end], ". ")
			if boundary > searchFrom {
				end = boundary + 1
			}
		}
		chunks = append(chunks, Chunk{Index: idx, Text: strings.TrimSpace(cleaned[start:end])})
		idx++
		// Once we've consumed the entire input, stop. Without this guard the loop
		// is infinite: end is capped at len(cleaned) and start = end - overlap is
		// still < len(cleaned), so the next iteration recomputes the same chunk.
		if end >= len(cleaned) {
			break
		}
		start = end - overlap
	}
	return chunks
}

var (
	reCodeFence = regexp.MustCompile("(?s)```.*?```")
	reInline    = regexp.MustCompile("`[^`]+`")
	reImage     = regexp.MustCompile(`!\[.*?\]\(.*?\)`)
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\(.*?\)`)
	reHeading   = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	reEmphasis  = regexp.MustCompile(`[*_]{1,3}([^*_]+)[*_]{1,3}`)
	reBullet    = regexp.MustCompile(`(?m)^\s*[-*+]\s+`)
	reOrdered   = regexp.MustCompile(`(?m)^\s*\d+\.\s+`)
	reBlockq    = regexp.MustCompile(`(?m)^>\s+`)
	reBlanks    = regexp.MustCompile(`\n{3,}`)
)

// StripMarkdown removes common Markdown syntax, leaving plain text for embedding.
func StripMarkdown(md string) string {
	s := md
	s = reCodeFence.ReplaceAllString(s, "")
	s = reInline.ReplaceAllString(s, "")
	s = reImage.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1")
	s = reHeading.ReplaceAllString(s, "")
	s = reEmphasis.ReplaceAllString(s, "$1")
	s = reBullet.ReplaceAllString(s, "")
	s = reOrdered.ReplaceAllString(s, "")
	s = reBlockq.ReplaceAllString(s, "")
	s = reBlanks.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
