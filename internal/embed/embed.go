// Package embed wraps Gemini's gemini-embedding-001 model.
package embed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"google.golang.org/genai"
)

const (
	EmbedModel = "gemini-embedding-001"
	EmbedDims  = 768
	GenModel   = "gemini-flash-latest"
)

// Client embeds text and generates answers using Gemini.
type Client struct {
	gen    *genai.Client
	policy retryPolicy
}

type retryPolicy struct {
	attempts        int
	backoff         time.Duration
	embedTimeout    time.Duration
	generateTimeout time.Duration
}

func (p retryPolicy) defaults() retryPolicy {
	if p.attempts <= 0 {
		p.attempts = 4
	}
	if p.backoff <= 0 {
		p.backoff = 500 * time.Millisecond
	}
	if p.embedTimeout <= 0 {
		p.embedTimeout = 20 * time.Second
	}
	if p.generateTimeout <= 0 {
		p.generateTimeout = 35 * time.Second
	}
	return p
}

// New creates a Client.
func New(ctx context.Context, apiKey string) (*Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not set")
	}
	g, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &Client{gen: g}, nil
}

// Gen returns the underlying client so callers can call other model APIs.
func (c *Client) Gen() *genai.Client { return c.gen }

// Generate streams an answer, retrying transient failures only before any text
// reaches yield. A successful answer must end normally and contain visible text.
func (c *Client) Generate(ctx context.Context, model, system, prompt string, yield func(string) error) error {
	if yield == nil {
		return fmt.Errorf("Gemini generation requires a text callback")
	}
	if model == "" {
		model = GenModel
	}
	contents := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		CandidateCount:    1,
		MaxOutputTokens:   1024,
	}
	return c.withRetry(ctx, c.policy.defaults().generateTimeout, func(attemptCtx context.Context) (bool, error) {
		delivered, hasText, stopped := false, false, false
		next, stop := iter.Pull2(c.gen.Models.GenerateContentStream(attemptCtx, model, contents, config))
		defer stop()
		for {
			chunk, err, ok := nextChunk(next)
			if err != nil {
				return !delivered, err
			}
			if !ok {
				break
			}
			if err := attemptCtx.Err(); err != nil {
				return !delivered, err
			}
			if chunk == nil {
				return false, fmt.Errorf("Gemini returned a nil response chunk")
			}
			if feedback := chunk.PromptFeedback; feedback != nil {
				if reason := feedback.BlockReason; reason != "" && reason != genai.BlockedReasonUnspecified {
					return false, fmt.Errorf("Gemini blocked the prompt: %s", reason)
				}
			}
			// Usage can arrive separately after the final candidate.
			if len(chunk.Candidates) == 0 && chunk.UsageMetadata != nil {
				continue
			}
			if len(chunk.Candidates) != 1 || chunk.Candidates[0] == nil {
				return false, fmt.Errorf("Gemini returned a malformed answer candidate")
			}
			candidate := chunk.Candidates[0]
			for _, rating := range candidate.SafetyRatings {
				if rating != nil && rating.Blocked {
					return false, fmt.Errorf("Gemini blocked the answer for safety")
				}
			}
			if stopped {
				return false, fmt.Errorf("Gemini returned answer content after its finish reason")
			}
			switch candidate.FinishReason {
			case "", genai.FinishReasonUnspecified:
			case genai.FinishReasonStop:
				stopped = true
			default:
				return false, fmt.Errorf("Gemini answer did not finish normally: %s", candidate.FinishReason)
			}
			var text strings.Builder
			if candidate.Content != nil {
				for _, part := range candidate.Content.Parts {
					if part == nil {
						return false, fmt.Errorf("Gemini returned a nil answer part")
					}
					if !part.Thought {
						text.WriteString(part.Text)
					}
				}
			}
			if s := text.String(); s != "" {
				// Once the callback sees text, replaying could duplicate an answer,
				// including when the callback itself returns an error.
				delivered = true
				hasText = hasText || strings.TrimSpace(s) != ""
				if err := yield(s); err != nil {
					return false, err
				}
			}
		}
		if err := attemptCtx.Err(); err != nil {
			return !delivered, err
		}
		if !stopped {
			// The SDK currently logs scanner failures instead of returning them.
			// Require STOP so a dropped connection cannot appear successful.
			return !delivered, fmt.Errorf("Gemini stream ended without a finish reason: %w", io.ErrUnexpectedEOF)
		}
		if !hasText {
			return false, fmt.Errorf("Gemini returned an empty answer")
		}
		return false, nil
	})
}

// One returns the embedding vector for a single piece of text.
func (c *Client) One(ctx context.Context, text string) ([]float32, error) {
	out, err := c.Batch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// Batch returns embeddings for many texts in one round trip.
// gemini-embedding-001 defaults to 3072 dims; we pin 768 to match the vec0
// schema (float[768]).
func (c *Client) Batch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	contents := make([]*genai.Content, len(texts))
	for i, t := range texts {
		contents[i] = genai.NewContentFromText(t, genai.RoleUser)
	}
	dims := int32(EmbedDims)
	var out [][]float32
	err := c.withRetry(ctx, c.policy.defaults().embedTimeout, func(attemptCtx context.Context) (bool, error) {
		res, err := func() (res *genai.EmbedContentResponse, err error) {
			defer malformedSDKResponse(&err)
			return c.gen.Models.EmbedContent(attemptCtx, EmbedModel, contents, &genai.EmbedContentConfig{
				OutputDimensionality: &dims,
			})
		}()
		if err != nil {
			return true, err
		}
		if res == nil || len(res.Embeddings) != len(texts) {
			return false, fmt.Errorf("Gemini returned an unexpected number of embeddings (want %d)", len(texts))
		}
		out = make([][]float32, len(texts))
		for i, e := range res.Embeddings {
			if e == nil || len(e.Values) != EmbedDims {
				return false, fmt.Errorf("Gemini embedding %d must contain %d dimensions", i, EmbedDims)
			}
			for _, v := range e.Values {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					return false, fmt.Errorf("Gemini embedding %d contains a non-finite value", i)
				}
			}
			out[i] = e.Values
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// The SDK's JSON converters can panic on null array elements. Keep that guard
// around SDK decoding only; application callbacks execute outside this boundary.
func malformedSDKResponse(err *error) {
	if value := recover(); value != nil {
		*err = fmt.Errorf("Gemini returned a malformed response: %v", value)
	}
}

func nextChunk(next func() (*genai.GenerateContentResponse, error, bool)) (chunk *genai.GenerateContentResponse, err error, ok bool) {
	defer malformedSDKResponse(&err)
	return next()
}

// withRetry bounds each request and retries only transient failures that can be
// replayed safely. The caller's context bounds the entire operation and backoff.
func (c *Client) withRetry(ctx context.Context, timeout time.Duration, operation func(context.Context) (bool, error)) error {
	p := c.policy.defaults()
	for attempt := 0; attempt < p.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		canReplay, err := operation(attemptCtx)
		if err == nil {
			err = attemptCtx.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if !canReplay || !transient(err) || attempt+1 == p.attempts {
			return fmt.Errorf("Gemini request failed on attempt %d: %w", attempt+1, err)
		}
		delay := p.backoff * time.Duration(1<<attempt)
		delay = time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("Gemini retry policy has no attempts")
}

func transient(err error) bool {
	var apiErr genai.APIError
	var apiErrPointer *genai.APIError
	code := 0
	if errors.As(err, &apiErr) {
		code = apiErr.Code
	} else if errors.As(err, &apiErrPointer) && apiErrPointer != nil {
		code = apiErrPointer.Code
	}
	if code != 0 {
		switch code {
		case 429, 500, 502, 503, 504:
			return true
		default:
			return false
		}
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}
