package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/embed"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Run the real CLI in a child process so a log.Fatal cannot terminate the test runner.
func TestIndexCLIHelperProcess(t *testing.T) {
	mode := os.Getenv("ASKMYBLOG_INDEX_HELPER")
	if mode == "" {
		return
	}
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if mode == "timeout" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return nil, errors.New("external API calls are disabled in index tests")
	})
	main()
	os.Exit(0)
}

func indexCommand(t *testing.T, mode, content string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	blog := filepath.Join(dir, "blog", "post")
	if err := os.MkdirAll(blog, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blog, "index.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIndexCLIHelperProcess$")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "ASKMYBLOG_INDEX_HELPER", "BLOG_PATH", "DB_PATH", "GEMINI_API_KEY", "INDEX_TIMEOUT":
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	timeout := "1m"
	if mode == "timeout" {
		timeout = "50ms"
	}
	cmd.Env = append(cmd.Env,
		"ASKMYBLOG_INDEX_HELPER="+mode,
		"BLOG_PATH="+filepath.Dir(blog),
		"DB_PATH="+filepath.Join(dir, "blog.db"),
		"GEMINI_API_KEY=test-key",
		"INDEX_TIMEOUT="+timeout,
	)
	return cmd, cancel
}

func TestIndexCLIFailsWhenAPostFails(t *testing.T) {
	cmd, cancel := indexCommand(t, "no-network", "---\ntitle: [invalid\n---\nBody text.")
	defer cancel()
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("indexing an invalid post exited successfully:\n%s", output)
	}
	if !strings.Contains(string(output), "frontmatter") {
		t.Fatalf("expected a frontmatter indexing failure, got %v:\n%s", err, output)
	}
}

func TestIndexCLIStopsAtConfiguredDeadline(t *testing.T) {
	cmd, cancel := indexCommand(t, "timeout", "---\ntitle: A post\n---\nBody text.")
	defer cancel()
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "deadline exceeded") {
		t.Fatalf("expected a bounded indexing deadline, got %v:\n%s", err, output)
	}
}

func TestIndexCLIFailsIfNoSearchableChunksWereIndexed(t *testing.T) {
	cmd, cancel := indexCommand(t, "no-network", "---\ntitle: Empty post\n---\n   \n")
	defer cancel()
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no indexed chunks") {
		t.Fatalf("expected an empty index to fail, got %v:\n%s", err, output)
	}
}

func TestIndexFileKeepsOldPostWhenALaterEmbeddingBatchFails(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vector := make([]float32, embed.EmbedDims)
	vector[0] = 1
	if _, err := store.Insert(db.Chunk{Slug: "post", Title: "Old post", Content: "Previously indexed content"}, vector); err != nil {
		t.Fatal(err)
	}
	postDir := filepath.Join(t.TempDir(), "post")
	if err := os.MkdirAll(postDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(postDir, "index.md")
	content := "---\ntitle: Replacement\n---\n" + strings.Repeat("Replacement content. ", 2500)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	calls := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":400,"message":"embedding failed","status":"INVALID_ARGUMENT"}}`)), Request: r}, nil
		}
		var request struct {
			Requests []json.RawMessage `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		values := make([]map[string]any, len(request.Requests))
		for i := range values {
			values[i] = map[string]any{"values": vector}
		}
		body, _ := json.Marshal(map[string]any{"embeddings": values})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
	})
	ec, err := embed.New(context.Background(), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := indexFile(context.Background(), store, ec, path); err == nil {
		t.Fatal("expected the second embedding batch to fail")
	}
	results, err := store.Search(vector, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "Previously indexed content" {
		t.Fatalf("failed embedding replaced the prior post: %+v", results)
	}
}
