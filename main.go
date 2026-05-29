// Command askmyblog-server exposes POST /api/ask as a streaming SSE endpoint
// and serves a tiny built-in chat page at /.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/embed"
	"github.com/oscillations-waves/askmyblog-go/internal/rag"
	"google.golang.org/genai"
)

func main() {
	dbPath := envOr("DB_PATH", "./blog.db")
	addr := ":" + envOr("PORT", "7860")
	allowed := splitCSV(os.Getenv("ALLOWED_ORIGIN"))

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

	engine := rag.NewEngine(store, ec)

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/ask", askHandler(engine, allowed))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// ────────── handlers ──────────

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!doctype html><meta charset="utf-8">
<title>askMyBlog</title>
<style>body{font:16px system-ui;max-width:680px;margin:2rem auto;padding:0 1rem}</style>
<h1>askMyBlog (Go backend)</h1>
<p>POST a JSON body <code>{"question":"…"}</code> to <code>/api/ask</code> to get a
text/event-stream response with <code>message</code>, <code>sources</code> and
<code>done</code> events.</p>`))
}

func askHandler(e *rag.Engine, allowed []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		setCORS(w, origin, allowed)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body struct {
			Question string `json:"question"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		body.Question = strings.TrimSpace(body.Question)
		if body.Question == "" {
			http.Error(w, "missing question", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		send := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		sendJSON := func(event string, v any) {
			b, _ := json.Marshal(v)
			send(event, string(b))
		}

		ctx := r.Context()
		chunks, err := e.Retrieve(ctx, body.Question, 5)
		if err != nil {
			sendJSON("error", err.Error())
			return
		}
		if len(chunks) == 0 {
			sendJSON("message", "I don't have enough information in my blog to answer that.")
			sendJSON("sources", []string{})
			send("done", "")
			return
		}

		prompt := rag.BuildPrompt(body.Question, chunks)
		contents := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}
		sysInst := genai.NewContentFromText(rag.SystemPrompt, genai.RoleUser)
		config := &genai.GenerateContentConfig{SystemInstruction: sysInst}

		stream := e.Embed.Gen().Models.GenerateContentStream(ctx, embed.GenModel, contents, config)
		for resp, err := range stream {
			if err != nil {
				sendJSON("error", err.Error())
				return
			}
			text := resp.Text()
			if text != "" {
				sendJSON("message", text)
			}
		}

		sendJSON("sources", rag.UniqueSlugs(chunks))
		send("done", "")
	}
}

// ────────── helpers ──────────

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setCORS(w http.ResponseWriter, origin string, allowed []string) {
	if origin == "" {
		return
	}
	if len(allowed) > 0 && !contains(allowed, origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Vary", "Origin")
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
