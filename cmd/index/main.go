// Command index walks BLOG_PATH, chunks each post and writes embeddings to DB_PATH.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/frontmatter"
	"github.com/oscillations-waves/askmyblog-go/internal/chunk"
	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/embed"
)

type meta struct {
	Title string   `yaml:"title"`
	Date  string   `yaml:"date"`
	Tags  []string `yaml:"tags"`
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n✅ Done.")
}

func run(ctx context.Context) (err error) {
	timeout, err := time.ParseDuration(envOr("INDEX_TIMEOUT", "10m"))
	if err != nil || timeout <= 0 {
		return fmt.Errorf("INDEX_TIMEOUT must be a positive duration, such as 10m")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	blogPath := envOr("BLOG_PATH", "../../shravani.roy/src/content/blog")
	dbPath := envOr("DB_PATH", "./blog.db")
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is not set")
	}

	store, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { err = errors.Join(err, store.Close()) }()

	ec, err := embed.New(ctx, apiKey)
	if err != nil {
		return fmt.Errorf("embed client: %w", err)
	}

	files, err := findMarkdown(blogPath)
	if err != nil {
		return fmt.Errorf("walk %s: %w", blogPath, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no markdown files in %s", blogPath)
	}

	fmt.Printf("\n🔍 Found %d post(s) in %s\n\n", len(files), blogPath)

	var failures []error
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("indexing stopped: %w", err)
		}
		if err := indexFile(ctx, store, ec, f); err != nil {
			log.Printf("  ✗ %s: %v", f, err)
			failures = append(failures, fmt.Errorf("%s: %w", f, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("indexing failed for %d of %d post(s): %w", len(failures), len(files), errors.Join(failures...))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("indexing stopped: %w", err)
	}
	count, err := store.Count()
	if err != nil {
		return fmt.Errorf("validate index: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("no indexed chunks; check that the blog contains searchable text")
	}
	return nil
}

func indexFile(ctx context.Context, store *db.DB, ec *embed.Client, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m meta
	body, err := frontmatter.Parse(strings.NewReader(string(raw)), &m)
	if err != nil {
		return fmt.Errorf("frontmatter: %w", err)
	}

	slug := filepath.Base(filepath.Dir(path))
	title := m.Title
	if title == "" {
		title = slug
	}
	date := ""
	if m.Date != "" {
		date = m.Date
	}
	tags := strings.Join(m.Tags, ", ")

	plain := chunk.StripMarkdown(string(body))
	chunks := chunk.Split(plain)
	if len(chunks) == 0 {
		fmt.Printf("  ⚠ %s — empty, skipping\n", slug)
		return nil
	}

	const batch = 20
	var embeddings [][]float32
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, end-i)
		for j, c := range chunks[i:end] {
			texts[j] = c.Text
		}
		vectors, err := ec.Batch(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed batch: %w", err)
		}
		if len(vectors) != len(texts) {
			return fmt.Errorf("embed batch: got %d vectors for %d chunks", len(vectors), len(texts))
		}
		embeddings = append(embeddings, vectors...)
	}

	// Finish every upstream request before replacing a previously indexed post.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ClearSlug(slug); err != nil {
		return fmt.Errorf("clear slug: %w", err)
	}
	for i, c := range chunks {
		_, err := store.Insert(db.Chunk{
			Slug:       slug,
			Title:      title,
			Date:       date,
			Tags:       tags,
			ChunkIndex: c.Index,
			Content:    c.Text,
		}, embeddings[i])
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	fmt.Printf("  ✓ %s — %d chunk(s)\n", slug, len(chunks))
	return nil
}

func findMarkdown(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".md" || ext == ".mdx" {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
