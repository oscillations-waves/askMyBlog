# syntax=docker/dockerfile:1.6
# Single-stage Go build for Hugging Face Spaces (Docker SDK).
# Pure-Go SQLite (ncruces) means no CGo and no native-module compile drama.

FROM golang:1.23-bookworm AS build
WORKDIR /src

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/askmyblog .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/index ./cmd/index

# Build the vector index using GEMINI_API_KEY as a BuildKit secret
# (configured in HF Space settings → Variables and secrets).
ARG BLOG_REPO=https://github.com/oscillations-waves/shravani.roy.git
ARG BLOG_BRANCH=main

RUN --mount=type=secret,id=GEMINI_API_KEY,required=true \
    git clone --depth 1 --branch "${BLOG_BRANCH}" "${BLOG_REPO}" /tmp/blog && \
    GEMINI_API_KEY="$(cat /run/secrets/GEMINI_API_KEY)" \
    BLOG_PATH=/tmp/blog/src/content/blog \
    DB_PATH=/out/blog.db \
    /out/index && \
    rm -rf /tmp/blog

# ─────────────── runtime ───────────────
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/askmyblog ./askmyblog
COPY --from=build /out/blog.db   ./blog.db

ENV PORT=7860
ENV DB_PATH=/app/blog.db
EXPOSE 7860

USER nonroot
ENTRYPOINT ["/app/askmyblog"]
