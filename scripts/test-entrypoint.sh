#!/bin/sh
set -eu

REPO_DIR=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT HUP INT TERM

fail() { echo "FAIL: $*" >&2; exit 1; }

fixture() {
  CASE_DIR="$TEST_DIR/$1"
  APP_DIR="$CASE_DIR/app"
  DB_PATH="$CASE_DIR/data/nested/blog.db"
  TRACE_FILE="$CASE_DIR/trace"
  mkdir -p "$APP_DIR/prebuilt" "$CASE_DIR/bin"
  : > "$TRACE_FILE"
  cat > "$CASE_DIR/bin/git" <<'EOF'
#!/bin/sh
for arg do destination=$arg; done
echo "clone:$destination" >> "$TRACE_FILE"
mkdir -p "$destination/src/content/blog"
EOF
  cat > "$APP_DIR/index" <<'EOF'
#!/bin/sh
echo "index:$DB_PATH" >> "$TRACE_FILE"
printf '%s' "${INDEX_TEXT:-complete}" > "$DB_PATH"
if [ "${INDEX_EXIT:-0}" != 0 ]; then
  printf 'partial journal' > "$DB_PATH-journal"
  exit "$INDEX_EXIT"
fi
EOF
  cat > "$APP_DIR/askmyblog" <<'EOF'
#!/bin/sh
echo "server:$DB_PATH" >> "$TRACE_FILE"
cat "$DB_PATH" > "$TRACE_FILE.served"
EOF
  chmod +x "$CASE_DIR/bin/git" "$APP_DIR/index" "$APP_DIR/askmyblog"
}

start() {
  env APP_DIR="$APP_DIR" DB_PATH="$DB_PATH" BLOG_REPO=https://example.invalid/blog.git \
    GEMINI_API_KEY=test-key TRACE_FILE="$TRACE_FILE" PATH="$CASE_DIR/bin:$PATH" \
    INDEX_EXIT="${1:-0}" sh "$REPO_DIR/entrypoint.sh" > "$CASE_DIR/output" 2>&1
}

fixture failed-index
mkdir -p "$(dirname "$DB_PATH")"
if start 1; then fail 'failed index returned success'; fi
grep -q '^index:' "$TRACE_FILE" || fail 'configured index executable was not run'
[ ! -e "$DB_PATH" ] || fail 'failed index published a partial database'
[ -z "$(find "$CASE_DIR/data" -type f -print)" ] || fail 'failed index left temporary database artifacts'
CLONE_DIR=$(sed -n 's/^clone://p' "$TRACE_FILE")
[ ! -d "$CLONE_DIR" ] || fail 'failed index left its clone directory'

fixture successful-index
start || { cat "$CASE_DIR/output"; fail 'startup with a new index failed'; }
[ "$(cat "$DB_PATH")" = complete ] || fail 'successful index was not published'
[ "$(cat "$TRACE_FILE.served")" = complete ] || fail 'server did not receive the published database'
INDEX_PATH=$(sed -n 's/^index://p' "$TRACE_FILE")
[ "$INDEX_PATH" != "$DB_PATH" ] || fail 'index was built in the published path'
[ ! -e "$INDEX_PATH" ] || fail 'successful index left its temporary database'
CLONE_DIR=$(sed -n 's/^clone://p' "$TRACE_FILE")
[ ! -d "$CLONE_DIR" ] || fail 'successful index left its clone directory'

fixture existing-index
mkdir -p "$(dirname "$DB_PATH")"
printf existing > "$DB_PATH"
start 1 || fail 'existing index was not reused'
[ "$(cat "$TRACE_FILE.served")" = existing ] || fail 'existing index changed'
if grep -Eq '^(index|clone):' "$TRACE_FILE"; then fail 'existing index triggered a rebuild'; fi

fixture prebuilt-index
printf packaged > "$APP_DIR/prebuilt/blog.db"
start 1 || fail 'prebuilt index was not loaded'
[ "$(cat "$TRACE_FILE.served")" = packaged ] || fail 'prebuilt database was not served'
if grep -Eq '^(index|clone):' "$TRACE_FILE"; then fail 'prebuilt index triggered a rebuild'; fi
[ -f "$APP_DIR/prebuilt/blog.db" ] || fail 'prebuilt source was moved instead of copied'

fixture empty-index
mkdir -p "$(dirname "$DB_PATH")"
: > "$DB_PATH"
start || fail 'empty database was not rebuilt'
[ "$(cat "$TRACE_FILE.served")" = complete ] || fail 'empty database was reused'

echo 'PASS: startup database publication, cleanup, reuse, and prebuilt index'
