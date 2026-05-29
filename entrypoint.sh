#!/bin/sh
set -e

# Index the blog on first start (or if the DB is missing).
# GEMINI_API_KEY is injected by HF Spaces at runtime, so no build secret needed.
if [ ! -f "$DB_PATH" ]; then
  echo "==> blog.db not found, indexing..."

  if [ -z "$GEMINI_API_KEY" ]; then
    echo "ERROR: GEMINI_API_KEY is not set. Add it in Space settings → Variables and secrets." >&2
    exit 1
  fi

  CLONE_DIR="$(mktemp -d)"
  git clone --depth 1 "${BLOG_REPO}" "${CLONE_DIR}"

  BLOG_PATH="${CLONE_DIR}/src/content/blog" \
    DB_PATH="${DB_PATH}" \
    /app/index

  rm -rf "${CLONE_DIR}"
  echo "==> Indexing complete."
fi

exec /app/askmyblog
