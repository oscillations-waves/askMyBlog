# syntax=docker/dockerfile:1
# Build stage: compile Go binaries (no CGo, pure-Go wasm sqlite).
FROM golang:1.25-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/askmyblog . && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/index ./cmd/index

# ─────────────── runtime ───────────────
# debian-slim gives us git + sh for the startup indexer without build secrets.
FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/askmyblog ./askmyblog
COPY --from=build /out/index     ./index
COPY entrypoint.sh               ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

ENV PORT=7860
ENV DB_PATH=/app/blog.db
ENV BLOG_REPO=https://github.com/oscillations-waves/shravani.roy.git
EXPOSE 7860

ENTRYPOINT ["/app/entrypoint.sh"]
