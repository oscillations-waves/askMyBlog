// Command askmyblog-server exposes POST /api/ask as a streaming SSE endpoint
// and serves a tiny built-in chat page at /.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/embed"
	"github.com/oscillations-waves/askmyblog-go/internal/rag"
)

func main() {
	if err := runServer(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func runServer(ctx context.Context) error {
	dbPath := envOr("DB_PATH", "./blog.db")
	addr := ":" + envOr("PORT", "7860")
	allowed := splitCSV(os.Getenv("ALLOWED_ORIGIN"))

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is not set")
	}

	store, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	ec, err := embed.New(ctx, apiKey)
	if err != nil {
		return fmt.Errorf("embed client: %w", err)
	}

	engine := rag.NewEngine(store, ec)
	engine.Model = envOr("GEMINI_MODEL", embed.GenModel)
	options := defaultAskOptions()
	if value := os.Getenv("REQUEST_TIMEOUT"); value != "" {
		options.timeout, err = time.ParseDuration(value)
		if err != nil || options.timeout <= 0 {
			return fmt.Errorf("REQUEST_TIMEOUT must be a positive duration, for example 60s")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/ask", askHandlerWithOptions(engine, allowed, options))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r.Header.Get("Origin"), allowed)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", readyHandler(store.Count, allowed))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	shutdown, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.BaseContext = func(net.Listener) context.Context { return shutdown }
	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("listening on %s model=%s request_timeout=%s", addr, engine.Model, options.timeout)
		serverErrors <- srv.ListenAndServe()
	}()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
	case <-shutdown.Done():
		deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(deadline); err != nil {
			log.Printf("shutdown: %v", err)
			srv.Close()
		}
	}
	return nil
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
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
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
