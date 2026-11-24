#!/bin/sh
set -eu

APP_DIR="${APP_DIR:-/app}"
if [ -z "${DB_PATH:-}" ]; then
  if [ -d /data ] && [ -w /data ]; then
    DB_PATH=/data/blog.db
  else
    DB_PATH="$APP_DIR/blog.db"
  fi
fi
export DB_PATH
mkdir -p "$(dirname "$DB_PATH")"

CLONE_DIR=
TEMP_DB=
cleanup() {
  if [ -n "$CLONE_DIR" ]; then
    rm -rf "$CLONE_DIR"
  fi
  if [ -n "$TEMP_DB" ]; then
    rm -f "$TEMP_DB" "$TEMP_DB-journal" "$TEMP_DB-wal" "$TEMP_DB-shm"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Reuse an existing index. A fresh one is published only after a successful build.
if [ ! -s "$DB_PATH" ]; then
  TEMP_DB="$(mktemp "${DB_PATH}.tmp.XXXXXX")"
  if [ -s "$APP_DIR/prebuilt/blog.db" ]; then
    echo "==> Loading prebuilt blog index."
    cp "$APP_DIR/prebuilt/blog.db" "$TEMP_DB"
  else
    echo "==> Blog index missing or empty, indexing..."
    if [ -z "${GEMINI_API_KEY:-}" ]; then
      echo "ERROR: GEMINI_API_KEY is not set. Add it in Space settings → Variables and secrets." >&2
      exit 1
    fi

    CLONE_DIR="$(mktemp -d)"
    git clone --depth 1 "${BLOG_REPO:-https://github.com/oscillations-waves/shravani.roy.git}" "$CLONE_DIR"
    BLOG_PATH="${CLONE_DIR}/src/content/blog" DB_PATH="$TEMP_DB" "$APP_DIR/index"
  fi

  if [ ! -s "$TEMP_DB" ]; then
    echo "ERROR: indexing did not produce a nonempty database." >&2
    exit 1
  fi
  mv -f "$TEMP_DB" "$DB_PATH"
  echo "==> Blog index ready."
fi

cleanup
trap - EXIT HUP INT TERM
exec "$APP_DIR/askmyblog"
