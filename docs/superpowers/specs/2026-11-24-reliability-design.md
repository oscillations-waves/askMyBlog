# Request reliability and latency

Improve the existing Go/Gemini blog backend's reported startup failures and slow answers. Keep the existing SSE `message`, `sources`, `error` (JSON string), and `done` events compatible with the blog frontend.

Approach: repair the current pipeline rather than change providers or hosting. Provider changes would require new credentials and quality checks; hosting changes require a deployment decision. Both remain optional operational choices.

Bound upstream retries and deadlines; retry generation only before answer text reaches the caller. Flush an initial SSE comment and periodic heartbeats. Send safe, actionable errors and log request IDs/stage durations. Validate bounded input. Cache only complete successful answers in a bounded, expiring in-memory cache. Add liveness/readiness endpoints.

Serialize the shared sqlite connection and make chunk/vector writes atomic. Build startup indexes in temporary databases, fail incomplete indexing runs, and publish only successful databases. Support an optional packaged index and mounted persistent storage to avoid re-embedding on each fresh container.

Defaults chosen for this repair: 60-second request deadline, 15-second heartbeat, 4,000-character questions, 16 KiB request bodies, 128 cached answers for 10 minutes, generation model configurable with the existing alias as fallback. No live provider calls or deployment changes are needed for verification.
