#!/bin/sh
set -e

# Index the published blog on first start (or if the DB is missing).
# GEMINI_API_KEY is injected by HF Spaces at runtime, so no build secret needed.
if [ ! -s "$DB_PATH" ]; then
  echo "==> blog.db not found, indexing..."

  if [ -z "$GEMINI_API_KEY" ]; then
    echo "ERROR: GEMINI_API_KEY is not set. Add it in Space settings → Variables and secrets." >&2
    exit 1
  fi

  TEMP_DB="$(mktemp "${DB_PATH}.tmp.XXXXXX")"
  trap 'rm -f "$TEMP_DB" "$TEMP_DB-journal"' EXIT
  trap 'exit 1' HUP INT TERM
  DB_PATH="$TEMP_DB" /app/index
  if [ ! -s "$TEMP_DB" ]; then
    echo "ERROR: indexing did not produce a database." >&2
    exit 1
  fi
  mv -f "$TEMP_DB" "$DB_PATH"
  trap - EXIT HUP INT TERM
  echo "==> Indexing complete."
fi

exec /app/askmyblog
