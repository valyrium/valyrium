# ADR 0003: Embedded dashboard + brand mark

## Status

Accepted and implemented. The design (docs/design/dashboard.html,
docs/design/logo-mark.svg) is embedded verbatim in internal/gateway/static/
and served at `GET /dashboard`.

Three visual directions were designed and compared; **"Reactor" was chosen**
— a deliberate single dark-theme commit (no light/dark toggle), a glowing
hero number for total requests, and glow-bordered cards. The token-usage
panel it displays is powered by a new persisted usage store — see
docs/adr/0004-usage-persistence.md, which this ADR depends on.

## Context

`valyrium` exposes `/metrics` (Prometheus text) and `/v1/models` (JSON), but
both are machine formats — there is no human-facing view of what the gateway
is doing (inflight requests, live tool-calling sessions, request volume by
route/status, configured models) without a Prometheus/Grafana stack, which is
a lot of ceremony for a single-binary, loopback-first tool. A lightweight,
built-in dashboard closes that gap.

Separately, the project has no visual identity — README, the brew tap page,
and the GitHub repo all render as plain text. A small brand mark was designed
alongside the dashboard so both ship together.

## Decision

Ship the dashboard as a single embedded static asset served by the existing
`valyrium` binary — no new API endpoints, no new runtime dependency, no
build step. The design artifacts are final and are to be embedded verbatim,
not redesigned:

- `docs/design/dashboard.html` — the complete page (HTML/CSS/JS, no external
  requests except same-origin `/metrics` and `/v1/models`, which it already
  calls).
- `docs/design/logo-mark.svg` — the brand mark, inlined in the dashboard's
  header and reusable standalone (README, brew tap, GitHub social image).

### Route

`GET /dashboard` returns `docs/design/dashboard.html`'s bytes verbatim,
`content-type: text/html; charset=utf-8`. It is wired in `ServeHTTP`
(`internal/gateway/server.go`) **before** the `isAuthorized` check — in the
same tier as `/healthz` — not after it like `/v1/models` and `/metrics`.

This is deliberate, not an oversight: the existing auth model is a request
header (`Authorization: Bearer <key>` or `x-api-key: <key>`), which an XHR/
`fetch()` call can set but a plain browser navigation to a URL cannot. Gating
`/dashboard` itself on that header would make it impossible to ever load the
page in a browser when `CLAUDE_GATEWAY_API_KEY` is set. Instead:

- `/dashboard` serves the static shell unauthenticated — it contains no data,
  only markup/CSS/JS.
- The shell's own JS calls the *already-authenticated* `/metrics` and
  `/v1/models` endpoints via `fetch()`, which can and does set the
  `Authorization` header once the user has supplied a key.
- On a `401`, the page shows an inline "API key required" field; the entered
  key is cached in `localStorage` (`valyrium.dashboardApiKey`) and attached to
  all subsequent polls. Nothing new is added to the server's auth logic.

No new JSON endpoints — the dashboard is a pure consumer of `/metrics` and
`/v1/models`, which already exist. `/metrics` does gain new gauge lines (the
token-usage metrics from docs/adr/0004-usage-persistence.md), but the
endpoint itself is unchanged.

### Finding: three routes were never actually recorded

While tracing exactly how the dashboard's "Requests by route" table would
get its data, `s.metrics.RecordRequest(...)` turned out to have exactly
**one** call site in the entire codebase: the `defer` in
`handleChatCompletions` (`internal/gateway/server.go`, around line 199).
`handleHealthz`, `handleModels`, and `handleMetrics` never call it — so
`llmgateway_requests_total` can, today, only ever contain a single route
(`POST /v1/chat/completions`), regardless of how many times `/healthz`,
`/v1/models`, or `/metrics` are hit. This is a real gap the dashboard design
exposed, not a hypothetical: without fixing it, the route table would be
permanently one row.

**Fix, bundled into this ADR's implementation** (small, same feature, not
scope creep): add a `start := time.Now()` and a
`defer func() { s.metrics.RecordRequest(r.Method, r.URL.Path, 200, time.Since(start)) }()`
to each of `handleHealthz`, `handleModels`, and `handleMetrics` — all three
currently only ever return `200`, so hardcoding that status is accurate. No
other behavior in those handlers changes.

### No new dependency (for this ADR specifically)

The page is one static file served byte-for-byte; embedding it needs only
`embed.FS` (standard library) and one `http.ResponseWriter.Write` call. This
ADR's own scope does not touch the dependency policy in docs/spec.md — the
one new dependency (`go.etcd.io/bbolt`) belongs to docs/adr/0004, which is a
separate, already-authorized exception.

## What implementation must do

1. Copy `docs/design/dashboard.html` and `docs/design/logo-mark.svg` byte-for-
   byte into `internal/gateway/static/` (a new directory) — `go:embed` cannot
   reach outside a package's own directory tree, so the design source
   (`docs/design/`) cannot be embedded in place.
2. Add a `//go:embed static/dashboard.html` (and, if the logo is ever served
   standalone rather than only inlined in the dashboard's `<svg>`, `static/
   logo-mark.svg`) directive and a handler that writes those bytes with the
   correct `content-type`.
3. Wire `GET /dashboard` into `ServeHTTP` ahead of the `isAuthorized` check,
   exactly like `/healthz`.
4. Add the missing `RecordRequest` instrumentation to `handleHealthz`,
   `handleModels`, and `handleMetrics` (see "Finding" above), so the route
   table reflects real traffic across all four recorded routes.
5. Implement docs/adr/0004-usage-persistence.md's usage store and metrics so
   the token-ledger panel reflects real, persisted numbers.
6. Do not alter the file's HTML/CSS/JS content — it is a finished design, not
   a draft. If a real gap is found during implementation (a metric that
   doesn't parse, a route that 404s), fix the bug without changing the
   visual design, and flag it back rather than silently reworking layout or
   copy.

## Known limitations / accepted tradeoffs

- No auth on the shell itself (see above) — this reveals no data, only the
  static page; accepted as the only way to make the page loadable by a plain
  browser navigation under the existing header-based auth model.
- The duration metric (`llmgateway_request_duration_seconds_sum/count`) is
  unlabeled in `internal/gateway/metrics.go` — global average latency only,
  no per-route latency. The dashboard reflects that; it does not invent
  per-route numbers the server doesn't emit.
- `/v1/models` does not indicate which model is the configured default
  (`CLAUDE_GATEWAY_MODEL` is not exposed by any endpoint) — the "Available
  models" panel lists what's advertised without marking a default, rather
  than guessing.
