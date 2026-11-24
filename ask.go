package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/oscillations-waves/askmyblog-go/internal/db"
	"github.com/oscillations-waves/askmyblog-go/internal/rag"
	"google.golang.org/genai"
)

type answerEngine interface {
	Retrieve(context.Context, string, int) ([]db.Result, error)
	Generate(context.Context, string, []db.Result, func(string) error) error
}

type askOptions struct {
	timeout   time.Duration
	heartbeat time.Duration
	cacheSize int
	cacheTTL  time.Duration
}

func defaultAskOptions() askOptions {
	return askOptions{timeout: 60 * time.Second, heartbeat: 15 * time.Second, cacheSize: 128, cacheTTL: 10 * time.Minute}
}

var requestSequence atomic.Uint64

func askHandler(e answerEngine, allowed []string) http.HandlerFunc {
	return askHandlerWithOptions(e, allowed, defaultAskOptions())
}

func askHandlerWithOptions(e answerEngine, allowed []string, options askOptions) http.HandlerFunc {
	cache := newAnswerCache(options.cacheSize, options.cacheTTL)
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := fmt.Sprintf("%x-%x", started.UnixNano(), requestSequence.Add(1))
		w.Header().Set("X-Request-ID", requestID)
		setCORS(w, r.Header.Get("Origin"), allowed)
		defer func() { log.Printf("request_id=%s duration=%s", requestID, time.Since(started)) }()
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, OPTIONS")
			writeJSONError(w, http.StatusMethodNotAllowed, "Use POST to ask a question.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		defer r.Body.Close()
		var body struct {
			Question string `json:"question"`
		}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil {
			writeBodyError(w, err)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeBodyError(w, err)
			return
		}
		body.Question = strings.TrimSpace(body.Question)
		if body.Question == "" || !utf8.ValidString(body.Question) || utf8.RuneCountInString(body.Question) > 4000 {
			writeJSONError(w, http.StatusBadRequest, "Enter a question between 1 and 4,000 characters.")
			return
		}
		if _, ok := w.(http.Flusher); !ok {
			writeJSONError(w, http.StatusInternalServerError, "Streaming is unavailable. Please try again later.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), options.timeout)
		defer cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		write := func(frame string) error {
			// Bound a slow/disconnected client's writes without putting an overall
			// WriteTimeout on a legitimately long-lived SSE response.
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			// HTTP/2 can reset an idle stream when this timer expires, so scope
			// the deadline to the write itself, not the next upstream wait.
			defer controller.SetWriteDeadline(time.Time{})
			if _, err := io.WriteString(w, frame); err != nil {
				return err
			}
			return controller.Flush()
		}
		send := func(event string, value any) error {
			data := ""
			if value != nil {
				encoded, err := json.Marshal(value)
				if err != nil {
					return err
				}
				data = string(encoded)
			}
			return write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
		}
		// Comments are ignored by SSE parsers, preserving the frontend contract.
		if err := write(": connected\n\n"); err != nil {
			return
		}
		if answer, ok := cache.get(body.Question); ok {
			log.Printf("request_id=%s cache=hit", requestID)
			if send("message", answer.text) != nil {
				return
			}
			if send("sources", answer.sources) != nil {
				return
			}
			_ = send("done", nil)
			return
		}
		events := make(chan answerEvent, 1)
		go streamAnswer(ctx, e, body.Question, requestID, events)
		ticker := time.NewTicker(options.heartbeat)
		defer ticker.Stop()
		var answer strings.Builder
		var sources []string
		for {
			select {
			case <-ctx.Done():
				if r.Context().Err() == nil {
					_ = send("error", publicError(ctx.Err()))
					_ = send("done", nil)
				}
				return
			case <-ticker.C:
				if write(": keep-alive\n\n") != nil {
					return
				}
			case event, ok := <-events:
				if !ok {
					return
				}
				if event.err != nil {
					log.Printf("request_id=%s error=%v", requestID, event.err)
					if send("error", publicError(event.err)) == nil {
						_ = send("done", nil)
					}
					return
				}
				if send(event.kind, event.data) != nil {
					return
				}
				switch event.kind {
				case "message":
					// Enforce a memory limit even if a provider ignores output limits.
					if answer.Len() <= 64*1024 {
						answer.WriteString(event.data.(string))
					}
				case "sources":
					sources = event.data.([]string)
				case "done":
					cache.put(body.Question, answer.String(), sources)
					return
				}
			}
		}
	}
}

type answerEvent struct {
	kind string
	data any
	err  error
}

func streamAnswer(ctx context.Context, e answerEngine, question, requestID string, events chan<- answerEvent) {
	defer close(events)
	emit := func(event answerEvent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case events <- event:
			return nil
		}
	}
	started := time.Now()
	chunks, err := e.Retrieve(ctx, question, 5)
	log.Printf("request_id=%s stage=retrieve duration=%s", requestID, time.Since(started))
	if err != nil {
		_ = emit(answerEvent{err: err})
		return
	}
	if len(chunks) == 0 {
		if emit(answerEvent{kind: "message", data: "I don't have enough information in my blog to answer that."}) != nil {
			return
		}
	} else {
		started = time.Now()
		err = e.Generate(ctx, question, chunks, func(text string) error {
			return emit(answerEvent{kind: "message", data: text})
		})
		log.Printf("request_id=%s stage=generate duration=%s", requestID, time.Since(started))
		if err != nil {
			_ = emit(answerEvent{err: err})
			return
		}
	}
	if emit(answerEvent{kind: "sources", data: rag.UniqueSlugs(chunks)}) != nil {
		return
	}
	_ = emit(answerEvent{kind: "done"})
}

func publicError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "The answer took too long. Please try again in a moment."
	}
	var apiError genai.APIError
	if errors.As(err, &apiError) {
		switch apiError.Code {
		case 429:
			return "The AI service is busy or its quota has been reached. Please try again shortly."
		case 500, 502, 503, 504:
			return "The AI service is temporarily unavailable. Please try again shortly."
		case 400, 401, 403, 404:
			return "The AI service is not configured correctly. Please contact the site owner."
		}
	}
	return "Unable to answer right now. Please try again shortly."
}

func writeBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "The request is too large.")
		return
	}
	writeJSONError(w, http.StatusBadRequest, "Send one JSON object containing a question.")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readyHandler(count func() (int, error), allowed []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r.Header.Get("Origin"), allowed)
		n, err := count()
		if err != nil || n == 0 {
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable, "The blog index is not ready.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "chunks": n})
	}
}
