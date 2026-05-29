// Package embed wraps Gemini's gemini-embedding-001 model.
package embed

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

const (
	EmbedModel = "gemini-embedding-001"
	EmbedDims  = 768
	GenModel   = "gemini-flash-latest"
)

// Client embeds text and generates answers using Gemini.
type Client struct {
	gen *genai.Client
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
	contents := make([]*genai.Content, len(texts))
	for i, t := range texts {
		contents[i] = genai.NewContentFromText(t, genai.RoleUser)
	}
	dims := int32(EmbedDims)
	res, err := c.gen.Models.EmbedContent(ctx, EmbedModel, contents, &genai.EmbedContentConfig{
		OutputDimensionality: &dims,
	})
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(res.Embeddings))
	for i, e := range res.Embeddings {
		out[i] = e.Values
	}
	return out, nil
}
