---
title: askMyBlog (Go)
emoji: 🐹
colorFrom: green
colorTo: blue
sdk: docker
app_port: 7860
pinned: false
---

# askMyBlog — Go backend

A Go rewrite of the askMyBlog RAG backend. Same architecture, smaller image,
no native-module compile drama.

## Stack

- **HTTP / SSE** — `net/http` standard library
- **Vector DB** — [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3) (pure-Go, no CGo) + [`sqlite-vec-go-bindings/ncruces`](https://github.com/asg017/sqlite-vec/tree/main/bindings/go)
- **Embeddings + LLM** — [`google/generative-ai-go`](https://github.com/google/generative-ai-go) (`text-embedding-004` + `gemini-1.5-flash`)
- **Frontmatter** — [`adrg/frontmatter`](https://github.com/adrg/frontmatter)

## Layout

```text
go-backend/
├── main.go                 # HTTP server: GET /, POST /api/ask (SSE), OPTIONS
├── cmd/index/main.go       # CLI indexer (mirrors scripts/index.ts)
├── internal/
│   ├── chunk/chunk.go      # Markdown strip + sentence-boundary chunker
│   ├── db/db.go            # sqlite + sqlite-vec wrapper
│   ├── embed/embed.go      # Gemini embedding client
│   └── rag/rag.go          # retrieve + prompt building
├── Dockerfile              # single-stage build, distroless runtime (~20 MB)
└── go.mod
```

## Local run

```sh
cd go-backend
go mod tidy

# Index your blog
GEMINI_API_KEY=sk-... \
BLOG_PATH=../../shravani.roy/src/content/blog \
DB_PATH=./blog.db \
go run ./cmd/index

# Start the server
GEMINI_API_KEY=sk-... \
ALLOWED_ORIGIN=http://localhost:4321 \
PORT=7860 \
go run .

# Test
curl -N -X POST http://localhost:7860/api/ask \
  -H 'Content-Type: application/json' \
  -d '{"question":"What did Shravani write about Ruby variables?"}'
```

## Environment

| Variable          | Default                                    | Purpose                                          |
| :---------------- | :----------------------------------------- | :----------------------------------------------- |
| `GEMINI_API_KEY`  | *(required)*                               | Google AI Studio API key                         |
| `DB_PATH`         | `./blog.db`                                | Path to the sqlite database                      |
| `PORT`            | `7860`                                     | HTTP listen port                                 |
| `ALLOWED_ORIGIN`  | *(unset → any origin allowed)*             | Comma-separated CORS allowlist                   |
| `BLOG_PATH`       | `../../shravani.roy/src/content/blog`      | Indexer-only: where to read posts from           |

## Deploying to Hugging Face Spaces

Same flow as the Node version:

1. Create a new Space (Docker SDK), e.g. `roybonny/askmyblog-go`.
2. Add `GEMINI_API_KEY` as a **secret** in Space settings.
3. Add `ALLOWED_ORIGIN` = `https://oscillations-waves.github.io` as a **variable**.
4. Push:
   ```sh
   git remote add hf-go https://huggingface.co/spaces/roybonny/askmyblog-go
   git subtree push --prefix=go-backend hf-go main
   ```

The Dockerfile bakes `blog.db` into the image at build time, then runs a
~20 MB distroless binary. Cold starts are noticeably faster than the Node
version.

## Why this exists

The Node version works fine — this is a portfolio piece showing the same RAG
architecture in Go: single static binary, no `node_modules`, no native-module
compile spikes, much faster cold starts on free-tier hosts.
