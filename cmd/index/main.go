// Command index walks BLOG_PATH, chunks each post and writes embeddings to DB_PATH.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

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
	blogPath := envOr("BLOG_PATH", "../../shravani.roy/src/content/blog")
	dbPath := envOr("DB_PATH", "./blog.db")
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY is not set")
	}

	ctx := context.Background()
	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	ec, err := embed.New(ctx, apiKey)
	if err != nil {
		log.Fatalf("embed client: %v", err)
	}

	files, err := findMarkdown(blogPath)
	if err != nil {
		log.Fatalf("walk %s: %v", blogPath, err)
	}
	if len(files) == 0 {
		log.Fatalf("no markdown files in %s", blogPath)
	}

	fmt.Printf("\n🔍 Found %d post(s) in %s\n\n", len(files), blogPath)

	for _, f := range files {
		if err := indexFile(ctx, store, ec, f); err != nil {
			log.Printf("  ✗ %s: %v", f, err)
		}
	}
	fmt.Println("\n✅ Done.")
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

	if err := store.ClearSlug(slug); err != nil {
		return fmt.Errorf("clear slug: %w", err)
	}

	const batch = 20
	inserted := 0
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, end-i)
		for j, c := range chunks[i:end] {
			texts[j] = c.Text
		}
		embeddings, err := ec.Batch(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed batch: %w", err)
		}
		for j, c := range chunks[i:end] {
			_, err := store.Insert(db.Chunk{
				Slug:       slug,
				Title:      title,
				Date:       date,
				Tags:       tags,
				ChunkIndex: c.Index,
				Content:    c.Text,
			}, embeddings[j])
			if err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			inserted++
		}
	}
	fmt.Printf("  ✓ %s — %d chunk(s)\n", slug, inserted)
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
