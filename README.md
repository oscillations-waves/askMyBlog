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

Answers questions about Shravani Roy's blog with retrieval augmented generation.
The server embeds a question, retrieves relevant passages from SQLite, and streams
a Gemini answer over server sent events (SSE).

## Stack and layout

- **HTTP / SSE:** Go's `net/http` standard library.
- **Vector storage:** [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3), using WebAssembly without CGo, with [`sqlite-vec`](https://github.com/asg017/sqlite-vec/tree/main/bindings/go).
- **Gemini SDK:** [`google.golang.org/genai`](https://github.com/googleapis/go-genai), using `gemini-embedding-001` with 768 dimensions and `gemini-flash-latest` for answers by default.
- **Frontmatter:** [`adrg/frontmatter`](https://github.com/adrg/frontmatter).

```text
.
├── main.go                 # Server configuration, CORS, health endpoints
├── ask.go                  # Request validation and SSE streaming
├── cache.go                # Bounded cache of completed answers
├── cmd/index/main.go       # Markdown/MDX indexing CLI
├── internal/
│   ├── chunk/chunk.go      # Markdown stripping and overlapping chunks
│   ├── db/db.go            # SQLite storage and vector search
│   ├── embed/              # Gemini embeddings, generation, and retries
│   └── rag/rag.go          # Retrieval and prompt construction
├── prebuilt/               # Optional prepared blog.db for the image
├── scripts/test-entrypoint.sh
├── entrypoint.sh           # Reuse, restore, or build an index before serving
└── Dockerfile              # Go build stage and Debian slim runtime
```

## Local run

Use Go 1.25 or newer. Set `GEMINI_API_KEY` in your shell before running either
command; the binaries do not load `.env` automatically.

```sh
# Index your local blog once. This calls the Gemini embedding API.
BLOG_PATH=/path/to/shravani.roy/src/content/blog \
DB_PATH=./blog.db \
go run ./cmd/index

# Start the server using that index.
ALLOWED_ORIGIN=http://localhost:4321 PORT=7860 go run .

curl -N -X POST http://localhost:7860/api/ask \
  -H 'Content-Type: application/json' \
  -d '{"question":"What did Shravani write about Ruby variables?"}'
```

The indexer exits unsuccessfully if any post fails. It finishes all embedding
batches for a post before deleting its previously indexed chunks, preserving that
post if an upstream embedding request fails. Stop the server before updating its
database; indexing and serving do not run concurrently.

## Environment

| Variable | Default | Purpose |
| :--- | :--- | :--- |
| `GEMINI_API_KEY` | Required | Google AI Studio API key, supplied as a runtime secret |
| `DB_PATH` | `./blog.db` for direct Go commands | SQLite path; container startup chooses `/data/blog.db` when `/data` exists and is writable, otherwise `/app/blog.db` |
| `PORT` | `7860` | HTTP listen port |
| `ALLOWED_ORIGIN` | Any origin | Comma-separated CORS allowlist |
| `GEMINI_MODEL` | `gemini-flash-latest` | Model used to generate answers |
| `REQUEST_TIMEOUT` | `60s` | Total deadline for retrieval, answer generation, and retries |
| `INDEX_TIMEOUT` | `10m` | Total deadline for the indexing CLI; accepts Go durations such as `15m` |
| `BLOG_PATH` | `../../shravani.roy/src/content/blog` | CLI source directory; container startup uses the cloned blog content |
| `BLOG_REPO` | `https://github.com/oscillations-waves/shravani.roy.git` | Repository cloned at startup when no reusable index exists |
| `APP_DIR` | `/app` | Installation directory containing the binaries and optional `prebuilt/blog.db` |

## Container startup and deployment

The runtime image contains the server and indexer binaries, Git, and CA
certificates. Startup reuses a nonempty database at `DB_PATH`. If none exists, it
copies an optional `/app/prebuilt/blog.db`, or clones the blog and runs the
indexer. New databases are built beside the destination and renamed into place
only on success. Failed builds stop startup and clean temporary files; the server
starts after indexing finishes.

To avoid embedding the entire blog on each fresh container, use a durable volume
or package a prepared index. For a durable volume:

```sh
docker build -t askmyblog .
docker run --rm -p 7860:7860 --env-file .env \
  -v askmyblog-data:/data askmyblog
```

An existing durable index takes precedence over a packaged one. To refresh it,
stop the service and replace or rebuild the database before restarting.

To package an index, generate it locally before building the image. This moves
embedding work out of container startup; the indexing command itself requires API
access, while packaging the generated database requires no embedding calls.

```sh
mkdir -p prebuilt
BLOG_PATH=/path/to/shravani.roy/src/content/blog \
DB_PATH=./prebuilt/blog.db \
go run ./cmd/index
docker build -t askmyblog .
```

The Dockerfile copies `prebuilt/` into the runtime image. Generated `blog.db`
files are ignored by Git; a deployment built from a Git checkout needs the
prepared file included explicitly, for example with `git add -f prebuilt/blog.db`.
The runtime still needs `GEMINI_API_KEY` to answer questions.

For Hugging Face Spaces, choose the Docker SDK, add `GEMINI_API_KEY` as a secret,
and set `ALLOWED_ORIGIN` to the blog's origin. Mount durable storage at `/data` if
available, or include a prepared index. Without either, every fresh container
must clone and embed the blog before it opens its HTTP port.

Hosting wake-up time remains separate from application processing; see
[Spaces sleep behavior](https://huggingface.co/docs/hub/en/spaces-gpus#sleep-time).
An HTTP 503 before the application responds can come from the hosting layer. A
provider error delivered inside an SSE stream comes from an already running
application; [Gemini troubleshooting](https://ai.google.dev/gemini-api/docs/troubleshooting)
describes provider availability and quota failures.

## Streaming and errors

`POST /api/ask` accepts one JSON object containing a question of 1–4,000
characters, with a maximum request body of 16 KiB. Successful streams emit JSON
strings in `message`, an array of post slugs in `sources`, and a final `done`
event. SSE comments are flushed immediately and every 15 seconds while waiting,
so the connection remains active during retrieval and generation.

Transient Gemini failures are retried up to four total attempts with exponential
backoff and jitter. Retryable cases include HTTP 429, 500, 502, 503, 504, and
transient network errors. Generation is retried only before answer text has been
delivered, preventing duplicate partial answers. Each embedding attempt has a
20-second deadline and each generation attempt has a 35-second deadline, within
the total request or indexing deadline.

Input errors return a JSON error with an HTTP 4xx status. Once streaming begins,
failures emit an `error` event containing a safe JSON string, followed by `done`.
Clients should handle both HTTP failures and SSE error events, stop their loading
state on errors, and offer a retry. Detailed provider errors stay in server logs.

Completed answers are cached for 10 minutes, up to 128 questions per server
process. Questions are trimmed but otherwise matched exactly. Failed and partial
answers are not cached. Responses include `X-Request-ID`; logs include that ID
and retrieval, generation, and total request durations.

`GET /healthz` returns HTTP 200 while the server is running. `GET /readyz` checks
that the blog index has matching stored chunks and vectors; it returns HTTP 503
with `Retry-After: 5` when the index is empty or unreadable. These endpoints do not
make Gemini API calls or guarantee provider availability.

If an older deployment left an empty or partial index, stop the service and
remove or regenerate that database. Startup reuses nonempty files and cannot
infer whether an older index includes every intended post.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
sh scripts/test-entrypoint.sh
```

The regression tests use local fake providers and temporary databases. They do
not make live Gemini calls.
