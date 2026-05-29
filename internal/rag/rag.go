// Package rag wires retrieval and answer generation together.
package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/embed"
)

const SystemPrompt = `You are a helpful assistant that answers questions about Shravani Roy's blog.
Answer ONLY using the provided excerpts below.
Do NOT include post slugs or citations in your answer — sources are shown separately by the UI.
If the excerpts don't contain enough information, say "I don't have enough information in my blog to answer that."
Keep your answer concise and accurate. Use markdown formatting where helpful.`

// Engine answers questions against an indexed blog.
type Engine struct {
	DB    *db.DB
	Embed *embed.Client
}

// NewEngine wires retrieval with the embedding/generation client.
func NewEngine(d *db.DB, e *embed.Client) *Engine {
	return &Engine{DB: d, Embed: e}
}

// Retrieve returns the top-k chunks nearest to the question.
func (e *Engine) Retrieve(ctx context.Context, question string, k int) ([]db.Result, error) {
	q, err := e.Embed.One(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	return e.DB.Search(q, k)
}

// BuildPrompt formats retrieved chunks into the user-facing prompt body.
func BuildPrompt(question string, chunks []db.Result) string {
	var b strings.Builder
	b.WriteString("Excerpts from my blog:\n\n")
	for i, c := range chunks {
		fmt.Fprintf(&b, "[%d] Slug: %s\nTitle: %s\n---\n%s\n\n", i+1, c.Slug, c.Title, c.Content)
	}
	fmt.Fprintf(&b, "Question: %s", question)
	return b.String()
}

// UniqueSlugs returns the set of slugs across results, preserving order.
func UniqueSlugs(chunks []db.Result) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, c := range chunks {
		if _, ok := seen[c.Slug]; ok {
			continue
		}
		seen[c.Slug] = struct{}{}
		out = append(out, c.Slug)
	}
	return out
}
