// Command index reads local Markdown or published articles and writes embeddings to DB_PATH.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	dbPath := envOr("DB_PATH", "./blog.db")
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is not set")
	}

	posts, err := loadPosts(ctx)
	if err != nil {
		return err
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

	fmt.Printf("\n🔍 Found %d post(s)\n\n", len(posts))
	for _, entry := range posts {
		if err := indexPost(ctx, store, ec, entry); err != nil {
			return fmt.Errorf("index %s: %w", entry.slug, err)
		}
	}
	return nil
}

func loadPosts(ctx context.Context) ([]post, error) {
	if site := os.Getenv("BLOG_SITE_URL"); site != "" && os.Getenv("BLOG_PATH") == "" {
		client := &http.Client{Timeout: 20 * time.Second}
		return fetchPublishedPosts(ctx, client, site)
	}

	blogPath := envOr("BLOG_PATH", "../../shravani.roy/src/content/blog")
	files, err := findMarkdown(blogPath)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", blogPath, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no markdown files in %s", blogPath)
	}

	posts := make([]post, 0, len(files))
	for _, f := range files {
		entry, err := readMarkdown(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		posts = append(posts, entry)
	}
	return posts, nil
}

func readMarkdown(path string) (post, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return post{}, err
	}
	var m meta
	body, err := frontmatter.Parse(strings.NewReader(string(raw)), &m)
	if err != nil {
		return post{}, fmt.Errorf("frontmatter: %w", err)
	}

	slug := filepath.Base(filepath.Dir(path))
	title := m.Title
	if title == "" {
		title = slug
	}
	return post{slug: slug, title: title, date: m.Date, tags: strings.Join(m.Tags, ", "), text: chunk.StripMarkdown(string(body))}, nil
}

func indexPost(ctx context.Context, store *db.DB, ec *embed.Client, entry post) error {
	chunks := chunk.Split(entry.text)
	if len(chunks) == 0 {
		fmt.Printf("  ⚠ %s — empty, skipping\n", entry.slug)
		return nil
	}

	if err := store.ClearSlug(entry.slug); err != nil {
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
		if len(embeddings) != len(texts) {
			return fmt.Errorf("embed batch: got %d vectors for %d chunks", len(embeddings), len(texts))
		}
		for j, c := range chunks[i:end] {
			_, err := store.Insert(db.Chunk{
				Slug:       entry.slug,
				Title:      entry.title,
				Date:       entry.date,
				Tags:       entry.tags,
				ChunkIndex: c.Index,
				Content:    c.Text,
			}, embeddings[j])
			if err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			inserted++
		}
	}
	fmt.Printf("  ✓ %s — %d chunk(s)\n", entry.slug, inserted)
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
