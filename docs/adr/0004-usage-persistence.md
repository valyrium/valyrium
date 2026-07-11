# ADR 0004: Persisted token/cost usage, via bbolt

## Status

Accepted (design). Not yet implemented.

## Context

The Reactor dashboard (ADR 0003, variant B of the design pass) includes a
token-usage panel showing input/output tokens and cost for five periods:
current, this week, this month, year to date, and all time. `valyrium`
currently computes per-request token/cost usage (`internal/gateway/openai.go`'s
`Usage`/`CostUSD` types, populated in `server.go`'s four completion-handling
paths) but never accumulates or persists it — each number is reported once,
in that request's own response, and then forgotten. There is nowhere to read
"this week's" total from.

Producing real week/month/YTD/all-time numbers needs two things `valyrium`
doesn't have today: **accumulation** (running sums, not just per-request
values) and **persistence** (surviving a restart — "this month" must include
requests served before the gateway's current process started).

Per docs/spec.md, the dependency policy is stdlib + `golang.org/x/...` only.
This ADR adds the one named exception the user authorized for this feature:
**`go.etcd.io/bbolt`**.

## Alternatives considered

- **Hand-rolled JSON file, rewritten periodically.** No new dependency, but a
  rewrite-the-whole-file approach risks corruption on a crash or power loss
  mid-write, and debouncing writes to bound I/O adds a data-loss window and
  extra scheduling logic to get right. Rejected once bbolt was authorized —
  bbolt gives real ACID transactions for less code, not more.
- **`mattn/go-sqlite3`.** Needs CGo, which breaks valyrium's simple
  cross-compiled single-binary distribution (the brew tap). Rejected.
- **`modernc.org/sqlite` (pure-Go SQLite transpile).** Avoids CGo, but pulls
  in a full relational engine (a large, slow-to-compile dependency) for an
  access pattern that is purely key-value (date → aggregate). Overkill.
- **`dgraph-io/badger` / `cockroachdb/pebble`.** LSM-tree engines built for
  high write throughput and large datasets, with background compaction
  workers. This workload is a handful of writes per completed request on a
  personal, local gateway — nowhere near the scale these are designed for,
  and the extra moving parts (compaction, level management) aren't worth it.
- **bbolt (`go.etcd.io/bbolt`).** Pure Go (no CGo — cross-compiles cleanly),
  a single embedded file, real ACID transactions (no torn writes), and a
  plain B+tree keyed exactly the way this data wants to be keyed (a date
  string). Chosen.

## Decision

### Schema

One bbolt database file, one bucket, dead simple:

- Bucket: `usage_days`
- Key: the **local calendar date** the request completed on, formatted
  `"2006-01-02"` (Go reference layout) — e.g. `"2026-07-10"`. Lexicographic
  byte ordering of this format is also chronological ordering, which bbolt's
  cursor iteration relies on.
- Value: JSON-encoded `{"input_tokens": <int64>, "output_tokens": <int64>, "cost_usd": <float64>}`.

No other tables, no schema versioning needed for a single flat bucket like
this.

### Write path

A new type in a new file, `internal/gateway/usage.go`:

```go
type UsageStore struct {
	db *bbolt.DB // nil if the store failed to open or is disabled — see "Best-effort" below
}
```

`RecordUsage(inputTokens, outputTokens int, costUSD *float64)`:
1. If `db == nil`, return immediately (no-op).
2. `db.Update(func(tx *bbolt.Tx) error { ... })`:
   - Get-or-create bucket `usage_days`.
   - Key = `time.Now().Format("2006-01-02")` (local time zone).
   - Read the existing value at that key (if present, unmarshal it; if
     absent, start from zero).
   - Add `inputTokens`, `outputTokens`, and `costUSD` (treat a nil `costUSD`
     as `0` — the CLI does not always report cost, and an undercounted total
     is preferable to guessing or erroring).
   - Marshal and `Put` back under the same key.

One call site: the existing `defer` in `handleChatCompletions`
(`internal/gateway/server.go` lines 197-212, right next to the existing
`s.metrics.RecordRequest(...)` call) already runs exactly once per HTTP
request/response and already closes over `promptTokens`/`completionTokens`
by reference — the same four code paths that set those two ints
(lines 298-299, 349-350, 528-529, 599-600) also each have a `*float64` cost
value available in scope (`completion.CostUSD` or `outcome.costUSD`). Thread
a third `costUSD *float64` pointer alongside the existing two, exactly
mirroring the existing pattern, and call
`s.metrics.usage.RecordUsage(promptTokens, completionTokens, costUSD)` in
that one defer — no new call sites, no new instrumentation elsewhere.

### Read / aggregation path

`WritePrometheus(w io.Writer)` on `UsageStore`, called from
`handleMetrics` alongside the existing `s.metrics.WritePrometheus(w)` call:

1. If `db == nil`, write nothing (the usage gauges are simply absent from
   `/metrics` output — never emitted as zero, so a scraper/dashboard can tell
   "disabled" from "genuinely zero usage").
2. `db.View(func(tx *bbolt.Tx) error { ... })`: iterate every key in
   `usage_days` with a cursor (bounded to ~366 entries per year of uptime —
   trivially fast to scan in full on every scrape; no separate in-memory
   cache is needed).
3. For each day, decide which of five periods it counts toward, relative to
   a reference time `now`. **The period-classification function takes `now
   time.Time` as an explicit parameter — it must not call `time.Now()`
   internally** — so it is independently testable against fixed reference
   dates (boundary cases: a day exactly 8 days ago must fall out of `week`
   but still count in `month`/`ytd`/`all`, etc.). `WritePrometheus` calls it
   with `time.Now()` at the real call site; tests call it directly with a
   fixed time. Periods are **cumulative windows**, not exclusive buckets —
   today's usage counts toward `current`, `week`, `month`, `ytd`, and `all`
   simultaneously:
   - `current`: the day equals `now`'s calendar date.
   - `week`: the day is on or after the current **ISO week's Monday**
     (`now`'s weekday, treating Sunday as 7, minus 1, subtracted from `now`,
     truncated to midnight).
   - `month`: the day is on or after the 1st of `now`'s month.
   - `ytd`: the day is on or after January 1st of `now`'s year.
   - `all`: unconditionally, every day.
4. Emit 15 gauge lines (3 metrics × 5 periods):

```
# HELP llmgateway_usage_input_tokens Cumulative input tokens by period
# TYPE llmgateway_usage_input_tokens gauge
llmgateway_usage_input_tokens{period="current"} 128400
llmgateway_usage_input_tokens{period="week"} 892000
llmgateway_usage_input_tokens{period="month"} 3100000
llmgateway_usage_input_tokens{period="ytd"} 18400000
llmgateway_usage_input_tokens{period="all"} 22000000
# HELP llmgateway_usage_output_tokens Cumulative output tokens by period
# TYPE llmgateway_usage_output_tokens gauge
llmgateway_usage_output_tokens{period="current"} 42150
... (week, month, ytd, all)
# HELP llmgateway_usage_cost_usd Cumulative cost in USD by period
# TYPE llmgateway_usage_cost_usd gauge
llmgateway_usage_cost_usd{period="current"} 1.86
... (week, month, ytd, all)
```

The period label values (`current`/`week`/`month`/`ytd`/`all`) are exactly
what the Reactor dashboard's token-ledger tabs already use — no translation
layer needed between the metric and the UI.

### Opening the store — best-effort, never fatal

`NewServer(config Config) *Server` keeps its existing signature (no error
return) — every existing test constructs a `*Server` this way and none of
them should need to change. Opening the bbolt file happens inside
`NewServer`, and a failure to open it is **not fatal**:

- On success: `UsageStore{db: theOpenedDB}`.
- On failure (permission error, disk full, corrupt file, etc.): log one
  warning line to stderr (`"warning: usage tracking disabled: %v"`) and
  construct `UsageStore{db: nil}`. The gateway starts and serves requests
  normally; usage tracking is simply off for that process. A non-critical
  analytics feature must never take down the primary gateway function.

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_USAGE_DB` | `$HOME/.valyrium/usage.db` (parent dir created with mode 0700 if missing) | Path to the bbolt file. Set to the literal string `off` to disable usage tracking entirely (no file created, `RecordUsage` a no-op, gauges omitted). Any other non-empty value is used as the literal file path. |

If `os.UserHomeDir()` errors (no `$HOME`), fall back to `./valyrium-usage.db`
in the current working directory, non-fatal.

### Shutdown

`Server.Shutdown(ctx)` (`internal/gateway/server.go`) additionally closes the
usage store's bbolt handle after the existing listener-close logic, so the
file isn't left with an open lock on a clean shutdown.

## What stays true

- No change to `NewServer`'s signature or to any existing test's
  construction of a `*Server`.
- No change to the four existing token/cost computation sites in
  `server.go` beyond threading one additional `*float64` parameter through
  the same call chain that already threads `*int` token pointers.
- `/metrics` gains new lines; nothing existing is removed or renamed.

## Known limitations / accepted tradeoffs

- **Local time zone.** Day boundaries (and therefore week/month/YTD
  boundaries) are the host machine's local time zone, not UTC. Documented,
  not hidden — a gateway that travels across time zones will see its
  "today" shift accordingly.
- **Single process, single file.** Like the rest of `valyrium`, this assumes
  one gateway process against one usage file. Running multiple replicas
  against the same file is unsupported (bbolt takes an exclusive file lock;
  a second process would fail to open it — which, per the best-effort
  policy above, degrades to "usage tracking disabled" for the second
  process rather than crashing it).
- **A crash between token computation and the deferred `RecordUsage` call is
  unrecorded** (the same window that already exists for the stderr request
  log right next to it) — accepted, this is analytics, not a ledger of
  record for billing reconciliation.
- **One new runtime dependency.** `go.etcd.io/bbolt` is the one named
  exception to the stdlib + `golang.org/x/...` policy, authorized
  specifically for this feature (see docs/spec.md).
