package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"google.golang.org/genai"
)

func TestServerHelperProcess(t *testing.T) {
	if os.Getenv("ASKMYBLOG_SERVER_HELPER") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func TestServerStartupFailureExitsNonzero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerHelperProcess$")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "ASKMYBLOG_SERVER_HELPER", "GEMINI_API_KEY", "DB_PATH", "PORT", "REQUEST_TIMEOUT":
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env, "ASKMYBLOG_SERVER_HELPER=1", "GEMINI_API_KEY=test-key", "DB_PATH="+filepath.Join(t.TempDir(), "blog.db"), "PORT=invalid-port", "REQUEST_TIMEOUT=60s")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "listen tcp") {
		t.Fatalf("expected startup failure exit, got %v:\n%s", err, output)
	}
}

func TestAskValidationReturnsJSON(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"invalid JSON", http.MethodPost, `{`, http.StatusBadRequest},
		{"missing question", http.MethodPost, `{"question":"  "}`, http.StatusBadRequest},
		{"wrong method", http.MethodGet, ``, http.StatusMethodNotAllowed},
		{"extra JSON", http.MethodPost, `{"question":"valid"} {}`, http.StatusBadRequest},
		{"long question", http.MethodPost, `{"question":"` + strings.Repeat("q", 4001) + `"}`, http.StatusBadRequest},
		{"large body", http.MethodPost, `{"question":"` + strings.Repeat("q", 17000) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			askHandler(nil, nil)(w, httptest.NewRequest(tc.method, "/api/ask", strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("expected JSON error, got %q", w.Body.String())
			}
			if body["error"] == "" {
				t.Fatal("missing user-facing error")
			}
		})
	}
}

type fakeEngine struct {
	retrieve    func(context.Context, string, int) ([]db.Result, error)
	generate    func(context.Context, string, []db.Result, func(string) error) error
	retrievals  atomic.Int32
	generations atomic.Int32
}

func (e *fakeEngine) Retrieve(ctx context.Context, question string, k int) ([]db.Result, error) {
	e.retrievals.Add(1)
	if e.retrieve != nil {
		return e.retrieve(ctx, question, k)
	}
	return []db.Result{{Chunk: db.Chunk{Slug: "post", Title: "A post", Content: "An excerpt"}}}, nil
}

func (e *fakeEngine) Generate(ctx context.Context, question string, chunks []db.Result, yield func(string) error) error {
	e.generations.Add(1)
	if e.generate != nil {
		return e.generate(ctx, question, chunks, yield)
	}
	return yield("An answer.")
}

func ask(t *testing.T, handler http.HandlerFunc, question string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"question": question})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(string(body))))
	return w
}

func TestAskCachesOnlyCompleteSuccessfulAnswers(t *testing.T) {
	e := &fakeEngine{}
	e.generate = func(ctx context.Context, question string, chunks []db.Result, yield func(string) error) error {
		if e.generations.Load() == 1 {
			if err := yield("Partial answer"); err != nil {
				return err
			}
			return genai.APIError{Code: 503, Message: "private provider detail"}
		}
		if err := yield("Complete "); err != nil {
			return err
		}
		return yield("answer.")
	}
	handler := askHandler(e, nil)
	first := ask(t, handler, "question")
	if !strings.Contains(first.Body.String(), `event: error`) || strings.Contains(first.Body.String(), "private provider detail") {
		t.Fatalf("expected safe error event: %s", first.Body)
	}
	if !strings.Contains(first.Body.String(), "event: done") || strings.Contains(first.Body.String(), "event: sources") {
		t.Fatalf("failure termination: %s", first.Body)
	}
	second := ask(t, handler, "question")
	third := ask(t, handler, " question ")
	if e.retrievals.Load() != 2 || e.generations.Load() != 2 {
		t.Fatalf("failed request cached or successful request missed cache: retrieve=%d generate=%d", e.retrievals.Load(), e.generations.Load())
	}
	if strings.Contains(second.Body.String(), "event: error") || !strings.Contains(third.Body.String(), `data: "Complete answer."`) {
		t.Fatalf("bad complete/cache answers: %s / %s", second.Body, third.Body)
	}
	if !strings.Contains(third.Body.String(), `data: ["post"]`) {
		t.Fatalf("missing cached sources: %s", third.Body)
	}
}

type observingWriter struct {
	*httptest.ResponseRecorder
	flushes chan string
}

func (w *observingWriter) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushes <- w.Body.String():
	default:
	}
}

func awaitFrame(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not flush")
		return ""
	}
}

func TestAskFlushesBeforeRetrievalAndSendsHeartbeats(t *testing.T) {
	release := make(chan struct{})
	e := &fakeEngine{retrieve: func(ctx context.Context, _ string, _ int) ([]db.Result, error) {
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	options := defaultAskOptions()
	options.heartbeat = 5 * time.Millisecond
	w := &observingWriter{ResponseRecorder: httptest.NewRecorder(), flushes: make(chan string, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		askHandlerWithOptions(e, nil, options)(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"question"}`)))
	}()
	if first := awaitFrame(t, w.flushes); first != ": connected\n\n" {
		t.Fatalf("initial flush = %q", first)
	}
	if next := awaitFrame(t, w.flushes); !strings.Contains(next, ": keep-alive\n\n") {
		t.Fatalf("heartbeat missing: %q", next)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete")
	}
	if e.generations.Load() != 0 {
		t.Fatal("empty retrieval should avoid generation")
	}
}

func TestAskDeadlineCancelsRetrieval(t *testing.T) {
	stopped := make(chan struct{})
	e := &fakeEngine{retrieve: func(ctx context.Context, _ string, _ int) ([]db.Result, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	options := defaultAskOptions()
	options.timeout = 20 * time.Millisecond
	w := ask(t, askHandlerWithOptions(e, nil, options), "question")
	if !strings.Contains(w.Body.String(), "took too long") || !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("timeout response: %s", w.Body)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream not cancelled")
	}
}

func TestAskClientCancellationStopsGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	e := &fakeEngine{generate: func(ctx context.Context, _ string, _ []db.Result, _ func(string) error) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		askHandler(e, nil)(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"question"}`)).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("generation not started")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("generation not stopped")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler not stopped")
	}
}

type brokenWriter struct {
	header http.Header
	writes int
}

func (w *brokenWriter) Header() http.Header { return w.header }
func (w *brokenWriter) WriteHeader(int)     {}
func (w *brokenWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("client disconnected")
	}
	return len(p), nil
}
func (w *brokenWriter) Flush() {}

func TestAskWriteFailureCancelsUpstream(t *testing.T) {
	stopped := make(chan struct{})
	e := &fakeEngine{generate: func(ctx context.Context, _ string, _ []db.Result, yield func(string) error) error {
		defer close(stopped)
		for {
			if err := yield("token"); err != nil {
				return err
			}
		}
	}}
	w := &brokenWriter{header: make(http.Header)}
	askHandler(e, nil)(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"question"}`)))
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("blocked upstream was not cancelled")
	}
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestAskClearsWriteDeadlineBetweenEvents(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	askHandler(&fakeEngine{}, nil)(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"question"}`)))
	if len(w.deadlines) == 0 {
		t.Fatal("stream writes were not bounded")
	}
	for i, deadline := range w.deadlines {
		if (i%2 == 1) != deadline.IsZero() {
			t.Fatalf("write deadline call %d = %s: expected arm/clear pairs", i, deadline)
		}
	}
	if !w.deadlines[len(w.deadlines)-1].IsZero() {
		t.Fatal("deadline remains armed while idle")
	}
}

func TestAskConcurrentCacheAccess(t *testing.T) {
	e := &fakeEngine{}
	handler := askHandler(e, nil)
	ask(t, handler, "question")
	done := make(chan bool, 20)
	for range 20 {
		go func() {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"question"}`)))
			done <- strings.Contains(w.Body.String(), `data: "An answer."`)
		}()
	}
	for range 20 {
		if !<-done {
			t.Fatal("missing cached answer")
		}
	}
	if e.generations.Load() != 1 {
		t.Fatal("cache miss during concurrent access")
	}
}

func TestAnswerCacheEvictionAndExpiration(t *testing.T) {
	c := newAnswerCache(2, time.Minute)
	c.put("one", "first", []string{"a"})
	c.put("two", "second", nil)
	if _, ok := c.get("one"); !ok {
		t.Fatal("missing answer")
	}
	c.put("three", "third", nil)
	if _, ok := c.get("two"); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	entry := c.items["one"]
	expired := entry.Value.(cachedAnswer)
	expired.expires = time.Now().Add(-time.Second)
	entry.Value = expired
	if _, ok := c.get("one"); ok {
		t.Fatal("expired answer was returned")
	}
	c.put("huge", strings.Repeat("x", 64*1024+1), nil)
	if _, ok := c.get("huge"); ok {
		t.Fatal("oversized answer was cached")
	}
}

func TestReadiness(t *testing.T) {
	for _, tc := range []struct {
		count  int
		err    error
		status int
	}{{0, nil, 503}, {3, nil, 200}, {0, errors.New("private database error"), 503}} {
		t.Run(fmt.Sprintf("%d-%d", tc.count, tc.status), func(t *testing.T) {
			w := httptest.NewRecorder()
			readyHandler(func() (int, error) { return tc.count, tc.err }, nil)(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private database error") {
				t.Fatalf("readiness: %d %s", w.Code, w.Body)
			}
			if tc.status == 503 && w.Header().Get("Retry-After") != "5" {
				t.Fatal("missing Retry-After")
			}
		})
	}
}

func TestAskCORSAndPreflight(t *testing.T) {
	handler := askHandler(nil, []string{"https://blog.example"})
	for _, origin := range []string{"https://blog.example", "https://other.example"} {
		r := httptest.NewRequest(http.MethodOptions, "/api/ask", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 204 {
			t.Fatalf("preflight status=%d", w.Code)
		}
		if (w.Header().Get("Access-Control-Allow-Origin") == origin) != (origin == "https://blog.example") {
			t.Fatal("CORS allowlist ignored")
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing request ID")
		}
	}
}
