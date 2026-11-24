package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, transport roundTripFunc) *Client {
	t.Helper()
	g, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: "test-key", Backend: genai.BackendGeminiAPI,
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{gen: g, policy: retryPolicy{backoff: time.Nanosecond}}
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func embeddingResponse(count, dims int) string {
	embeddings := make([]map[string]any, count)
	for i := range embeddings {
		values := make([]float32, dims)
		if dims > 0 {
			values[0] = 0.25
		}
		embeddings[i] = map[string]any{"values": values}
	}
	data, _ := json.Marshal(map[string]any{"embeddings": embeddings})
	return string(data)
}

func TestOneRejectsMissingEmbedding(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		return httpResponse(200, `{}`), nil
	})
	if _, err := c.One(context.Background(), "question"); err == nil {
		t.Fatal("missing embeddings must return an error")
	}
}

func TestOneRetriesUnavailableAndReturnsVector(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 4 {
			return httpResponse(503, `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`), nil
		}
		return httpResponse(200, embeddingResponse(1, 768)), nil
	})
	vector, err := c.One(context.Background(), "question")
	if err != nil {
		t.Fatalf("expected recovery from three transient errors, got %v", err)
	}
	if len(vector) != 768 || vector[0] != 0.25 || attempts != 4 {
		t.Fatalf("vector dimensions=%d, attempts=%d", len(vector), attempts)
	}
}

func streamResponse(chunks ...string) *http.Response {
	res := httpResponse(200, strings.Join(chunks, "\n\n")+"\n\n")
	res.Header.Set("Content-Type", "text/event-stream")
	return res
}

const completeAnswer = `data: {"candidates":[{"content":{"parts":[{"text":"An answer."}],"role":"model"},"finishReason":"STOP","index":0}]}`

func TestGenerateRetriesBeforeTextAndUsesRequestedModel(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if !strings.Contains(r.URL.Path, "models/custom-model:streamGenerateContent") {
			t.Errorf("unexpected generation model: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		config, ok := body["generationConfig"].(map[string]any)
		if !ok || config["maxOutputTokens"] != float64(1024) {
			t.Errorf("missing output token bound: %v", body)
		}
		if _, set := config["thinkingConfig"]; set {
			t.Error("must leave model-specific thinking settings at their defaults")
		}
		instruction, _ := body["systemInstruction"].(map[string]any)
		parts, _ := instruction["parts"].([]any)
		if len(parts) != 1 || parts[0].(map[string]any)["text"] != "use excerpts" {
			t.Errorf("missing system instruction: %v", body)
		}
		if attempts < 4 {
			return httpResponse(503, `{"error":{"code":503,"message":"overloaded"}}`), nil
		}
		return streamResponse(completeAnswer), nil
	})
	var answer strings.Builder
	err := c.Generate(context.Background(), "custom-model", "use excerpts", "question", func(text string) error {
		answer.WriteString(text)
		return nil
	})
	if err != nil || answer.String() != "An answer." || attempts != 4 {
		t.Fatalf("answer=%q attempts=%d err=%v", answer.String(), attempts, err)
	}
}

func TestGenerateNeverReplaysDeliveredText(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		return streamResponse(
			`data: {"candidates":[{"content":{"parts":[{"text":"Partial"}],"role":"model"},"index":0}]}`,
			`{"error":{"code":503,"message":"interrupted"}}`,
		), nil
	})
	var got []string
	err := c.Generate(context.Background(), GenModel, "system", "question", func(text string) error {
		got = append(got, text)
		return nil
	})
	if err == nil || attempts != 1 || strings.Join(got, "") != "Partial" {
		t.Fatalf("text=%q attempts=%d err=%v", got, attempts, err)
	}
}

func TestGenerateDoesNotExposeThoughtsAndCanRetryThem(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return streamResponse(
				`data: {"candidates":[{"content":{"parts":[{"text":"private thought","thought":true}],"role":"model"},"index":0}]}`,
				`{"error":{"code":503,"message":"interrupted"}}`,
			), nil
		}
		return streamResponse(completeAnswer), nil
	})
	var answer strings.Builder
	err := c.Generate(context.Background(), GenModel, "system", "question", func(text string) error {
		answer.WriteString(text)
		return nil
	})
	if err != nil || answer.String() != "An answer." || attempts != 2 {
		t.Fatalf("answer=%q attempts=%d err=%v", answer.String(), attempts, err)
	}
}

func TestGenerateRejectsUnsuccessfulStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"missing candidates", `data: {}`},
		{"null candidate", `data: {"candidates":[null]}`},
		{"null part", `data: {"candidates":[{"content":{"parts":[null]},"finishReason":"STOP"}]}`},
		{"blocked prompt", `data: {"promptFeedback":{"blockReason":"SAFETY"}}`},
		{"blocked output", `data: {"candidates":[{"finishReason":"SAFETY"}]}`},
		{"empty answer", `data: {"candidates":[{"finishReason":"STOP"}]}`},
		{"no finish", `data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`},
		{"truncated", `data: {"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}]}`},
		{"invalid JSON", `data: {not json}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				return streamResponse(tc.data), nil
			})
			if err := c.Generate(context.Background(), GenModel, "system", "question", func(string) error { return nil }); err == nil {
				t.Fatal("unsuccessful stream must return an error")
			}
		})
	}
}

func TestGenerateReturnsCallbackErrorWithoutRetry(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		return streamResponse(completeAnswer), nil
	})
	want := errors.New("client disconnected")
	err := c.Generate(context.Background(), GenModel, "system", "question", func(string) error { return want })
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestBatchRejectsMalformedVectors(t *testing.T) {
	for _, tc := range []struct {
		name, response string
	}{
		{"missing", `{}`},
		{"null", `{"embeddings":[null]}`},
		{"extra", embeddingResponse(2, 768)},
		{"wrong dimensions", embeddingResponse(1, 3)},
		{"overflow", `{"embeddings":[{"values":[1e100]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			c := testClient(t, func(r *http.Request) (*http.Response, error) {
				attempts++
				return httpResponse(200, tc.response), nil
			})
			if _, err := c.Batch(context.Background(), []string{"question"}); err == nil {
				t.Fatal("malformed embeddings must return an error")
			}
			if attempts != 1 {
				t.Fatalf("malformed embeddings must not be retried, got %d attempts", attempts)
			}
		})
	}
}

func TestBatchPinsDimensionsAndPreservesMultipleInputs(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		var body struct {
			Requests []struct {
				OutputDimensionality int `json:"outputDimensionality"`
				Content              struct {
					Parts []struct{ Text string } `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Requests) != 2 {
			t.Fatalf("expected two embedding requests, got %d", len(body.Requests))
		}
		for i, want := range []string{"first", "second"} {
			request := body.Requests[i]
			if request.OutputDimensionality != 768 || len(request.Content.Parts) != 1 || request.Content.Parts[0].Text != want {
				t.Errorf("incorrect embedding request %d: %+v", i, request)
			}
		}
		return httpResponse(200, embeddingResponse(2, 768)), nil
	})
	vectors, err := c.Batch(context.Background(), []string{"first", "second"})
	if err != nil || len(vectors) != 2 || len(vectors[0]) != 768 || len(vectors[1]) != 768 {
		t.Fatalf("vectors=%v err=%v", vectors, err)
	}
}

func TestTransientStatusRetriesAreBounded(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 429, 500, 502, 503, 504} {
		for _, operation := range []string{"embed", "generate"} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				attempts := 0
				c := testClient(t, func(r *http.Request) (*http.Response, error) {
					attempts++
					return httpResponse(status, fmt.Sprintf(`{"error":{"code":%d,"message":"failed"}}`, status)), nil
				})
				var err error
				if operation == "embed" {
					_, err = c.One(context.Background(), "question")
				} else {
					err = c.Generate(context.Background(), GenModel, "system", "question", func(string) error { return nil })
				}
				want := 1
				if status == 429 || status >= 500 {
					want = 4
				}
				var apiErr genai.APIError
				if !errors.As(err, &apiErr) || apiErr.Code != status || attempts != want {
					t.Fatalf("attempts=%d want=%d err=%v", attempts, want, err)
				}
			})
		}
	}
}

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary network failure" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func TestOneRetriesTemporaryNetworkFailures(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, temporaryError{}
		}
		return httpResponse(200, embeddingResponse(1, 768)), nil
	})
	vector, err := c.One(context.Background(), "question")
	if err != nil || len(vector) != 768 || attempts != 3 {
		t.Fatalf("dimensions=%d attempts=%d err=%v", len(vector), attempts, err)
	}
}

func TestOneCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		time.AfterFunc(5*time.Millisecond, cancel)
		return httpResponse(503, `{"error":{"code":503,"message":"overloaded"}}`), nil
	})
	c.policy.backoff = time.Hour
	start := time.Now()
	_, err := c.One(ctx, "question")
	if !errors.Is(err, context.Canceled) || attempts != 1 || time.Since(start) > time.Second {
		t.Fatalf("attempts=%d elapsed=%s err=%v", attempts, time.Since(start), err)
	}
}

func TestOneBoundsEachAttempt(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c.policy.embedTimeout = 5 * time.Millisecond
	start := time.Now()
	_, err := c.One(context.Background(), "question")
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 4 || time.Since(start) > time.Second {
		t.Fatalf("attempts=%d elapsed=%s err=%v", attempts, time.Since(start), err)
	}
}

type stalledBody struct{ ctx context.Context }

func (s stalledBody) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s stalledBody) Close() error { return nil }

func TestGenerateBoundsStalledStream(t *testing.T) {
	attempts := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		res := streamResponse()
		res.Body = stalledBody{ctx: r.Context()}
		return res, nil
	})
	c.policy.generateTimeout = 5 * time.Millisecond
	start := time.Now()
	err := c.Generate(context.Background(), GenModel, "system", "question", func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || attempts != 4 || time.Since(start) > time.Second {
		t.Fatalf("attempts=%d elapsed=%s err=%v", attempts, time.Since(start), err)
	}
}

func TestGenerateHonorsCallerDeadline(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := c.Generate(ctx, GenModel, "system", "question", func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller deadline, got %v", err)
	}
}
