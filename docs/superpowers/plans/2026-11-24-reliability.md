# Request Reliability Implementation Plan

> **For agentic workers:** Use test-driven changes and independent review. Steps use checkboxes for progress.

**Goal:** Make upstream failures recoverable and diagnosable, reduce repeated-answer latency, and remove avoidable cold-start work.

**Architecture:** Preserve the HTTP/SSE contract and current provider. Bound expensive work, cache complete answers, and safely reuse indexes.

**Tech Stack:** Go, net/http, google.golang.org/genai v1.58.0, ncruces sqlite and sqlite-vec.

**Spec:** ../specs/2026-11-24-reliability-design.md

## Constraints and review focus

- Never retry generation after visible output or cache incomplete/error responses.
- Cancellation must stop waiting, upstream work, and blocked stream writes.
- Preserve SSE error strings for the existing frontend; hide provider internals.
- No partial index publication, orphan chunks, or concurrent sqlite connection use.
- No deployment, paid model calls, or new dependencies. Branch/PR publication is authorized by the user's follow-up request.

## Tasks

- [x] Add regression tests for malformed provider output, transient failures, retry exhaustion, cancellation, and stream interruptions; implement bounded embedding/generation retries in `internal/embed`.
- [x] Add real sqlite tests for concurrent access, counts and insert rollback; implement serialization and transactions in `internal/db`.
- [x] Add startup/indexer regression tests; atomically publish complete indexes and support optional prebuilt/persistent databases; correct deployment docs.
- [x] Add HTTP tests for validation, safe errors, immediate streaming, timeouts, cancellation, and success-only caching. Introduce a small engine interface, health endpoints, request logs, stream heartbeats, bounded cache, and graceful shutdown in the server.
- [x] Run `go test -race ./...`, `go vet ./...`, build both binaries and check shell syntax. Have a fresh reviewer inspect the integrated patch and resolve material findings.

## Execution notes

Baseline tests initially could not access the default Go cache; temporary caches and downloaded existing dependencies resolve the environment restriction. Startup, DB, and provider work are delegated with non-overlapping file ownership. HTTP integration stays with the primary agent. Changes stay in the user's checkout for review.

Final verification: all packages passed `go test -race ./...`; `go vet ./...`, both `CGO_ENABLED=0` binary builds, shell syntax/startup regression tests, and `git diff --check` passed. Independent review identified an idle HTTP/2 write-deadline hazard (fixed with a failing/passing deadline regression) and ignored prebuilt-index packaging instructions (corrected to force-add deliberately). Startup listener failures also have a failing/passing exit-status regression. No live Gemini calls or production deployment occurred. Actual production 503 origin and latency remain unmeasured.
