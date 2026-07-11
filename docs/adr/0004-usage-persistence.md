# ADR 0004: Persisted token/cost usage

## Status

Accepted and implemented (internal/gateway/usage.go).

Amended 2026-07-10: the store was first built on `go.etcd.io/bbolt`, authorized
as a one-off exception to the dependency policy. That exception has been
withdrawn and the store rebuilt on the standard library — see "Why not bbolt,
after all" below. `valyrium` is back to zero third-party modules.

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

Per docs/spec.md, the dependency policy is stdlib + `golang.org/x/...` only,
and this feature does not change that.

## Alternatives considered

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
- **bbolt (`go.etcd.io/bbolt`).** Chosen first, then reverted. See below.
- **A JSON file held in memory and rewritten atomically.** No dependency at
  all. Chosen.

## Why not bbolt, after all

bbolt was the right shape for the job on its own merits — pure Go, one
embedded file, ACID transactions, a B+tree keyed exactly how this data wants
to be keyed. What killed it was its *module graph*, not its code.

`go.etcd.io/bbolt`'s own `go.mod` requires `stretchr/testify`, `go.etcd.io/gofail`,
`spf13/cobra` and friends — dependencies of its test suite and its `bbolt` CLI,
none of which valyrium compiles or ships. But Go's module graph is not the
import graph: `go list -m all` reports the requirements declared by every
dependency's `go.mod`, whether or not any of their packages are built. Adding
bbolt therefore turned valyrium's dependency list from two modules into eleven,
including four `github.com/...` modules the dependency policy exists to keep out.
This is not fixable by pinning: every bbolt release from v1.3.7 onward declares
those requirements (only the 2021-era v1.3.6 does not, and pinning to an
unmaintained release to satisfy a lint is a worse trade than writing the ~60
lines below).

So the choice was: keep a dependency whose declared graph violates the policy in
a way no configuration can suppress, or write the store by hand. The store is
small enough that writing it by hand won, and the original objection to doing so
turns out not to hold:

- **"A full rewrite risks corruption on a crash mid-write."** Only if the
  rewrite is done in place. Writing to a temp file in the same directory,
  `fsync`ing it, and `rename`ing it over the target is atomic on POSIX: a crash
  at any point leaves either the old complete file or the new complete file,
  never a torn one. That is the same durability property bbolt's transactions
  were wanted for.
- **"Debouncing writes to bound I/O adds a data-loss window."** There is no
  debouncing, because there is nothing to bound. The ledger is one entry per
  calendar day — roughly 30 KB after a year of continuous use — and it is
  rewritten on each completed request, which on a personal local gateway is a
  handful per minute at most. Writing the whole file every time is cheaper than
  the machinery that would avoid it, and it leaves no window at all: a request
  is on disk before its response is logged.

## Decision

### Schema

One JSON file, one object, dead simple:

```json
{"days": {"2026-07-10": {"input_tokens": 128400, "output_tokens": 42150, "cost_usd": 1.86}}}
```

- Key: the **local calendar date** the request completed on, formatted
  `"2006-01-02"` (Go reference layout) — e.g. `"2026-07-10"`.
- Value: `{"input_tokens": <int64>, "output_tokens": <int64>, "cost_usd": <float64>}`.

One entry per day, so the file stays around 30 KB per year of continuous use.
No schema versioning needed for a single flat map like this; an unparseable
file disables tracking rather than being overwritten (see "Best-effort" below),
so a future format change can detect and migrate rather than destroy.

### Write path

A new type in a new file, `internal/gateway/usage.go`:

```go
type UsageStore struct {
	mu   sync.Mutex
	path string
	days map[string]DayUsage // nil if the store failed to open or is disabled
}
```

The whole ledger is held in memory — it is one entry per day, and the read path
scans all of it on every scrape anyway — so a record is a map update plus a
rewrite of the file.

`RecordUsage(inputTokens, outputTokens int, costUSD *float64)`:
1. If `days == nil`, return immediately (no-op).
2. Under `mu`:
   - Key = `time.Now().Format("2006-01-02")` (local time zone).
   - Add `inputTokens`, `outputTokens`, and `costUSD` to that day's entry,
     starting from zero if it is the first record of the day (treat a nil
     `costUSD` as `0` — the CLI does not always report cost, and an
     undercounted total is preferable to guessing or erroring).
   - Rewrite the file: marshal the whole map, write it to a temp file in the
     same directory, `fsync`, `chmod 0600`, and `rename` it over the target.
     The rename is atomic, so a crash mid-write leaves the previous ledger
     intact rather than a half-written one. A write failure is logged, not
     returned — the request it belongs to has already been served.

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

1. If `days == nil`, write nothing (the usage gauges are simply absent from
   `/metrics` output — never emitted as zero, so a scraper/dashboard can tell
   "disabled" from "genuinely zero usage").
2. Under `mu`, iterate every entry in `days` (bounded to ~366 entries per year
   of uptime — trivially fast to scan in full on every scrape; no separate
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
them should need to change. Opening the ledger happens inside `NewServer`, and
a failure to open it is **not fatal**:

- On success: the file is read into `days` (a missing file is an empty ledger,
  not an error — that is simply the first run) and written straight back out.
  That opening write is deliberate: a store that cannot persist should announce
  itself at startup, not on the first completed request.
- On failure (permission error, disk full, unparseable file, etc.): log one
  warning line to stderr (`"warning: usage tracking disabled: %v"`) and leave
  `days` nil. The gateway starts and serves requests normally; usage tracking
  is simply off for that process. A non-critical analytics feature must never
  take down the primary gateway function. Note that an unparseable file is
  *disabling*, not overwritten — a corrupt ledger is a bug to look at, not
  data to silently discard.

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_USAGE_DB` | `$HOME/.valyrium/usage.db` (parent dir created with mode 0700 if missing) | Path to the usage ledger, a JSON file written with mode 0600. Set to the literal string `off` to disable usage tracking entirely (no file created, `RecordUsage` a no-op, gauges omitted). Any other non-empty value is used as the literal file path. |

If `os.UserHomeDir()` errors (no `$HOME`), fall back to `./valyrium-usage.db`
in the current working directory, non-fatal.

### Shutdown

`Server.Shutdown(ctx)` (`internal/gateway/server.go`) additionally closes the
usage store after the existing listener-close logic, which flushes the ledger
one last time. Every record already writes through to disk, so this is a
belt-and-braces flush rather than the only durability point.

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
- **Single process, single file — and now unenforced.** Like the rest of
  `valyrium`, this assumes one gateway process against one usage file. bbolt
  took an exclusive file lock, so a second process against the same file would
  have failed to open it and degraded to "tracking disabled". There is no lock
  here: two processes sharing a ledger will each hold their own copy in memory
  and clobber each other's totals on write, last writer wins. This is a real
  regression against the bbolt design, accepted because running two valyrium
  processes against one usage file is already unsupported and because the
  failure is bounded to analytics — no request is affected. Point a second
  process at a different `CLAUDE_GATEWAY_USAGE_DB`, or at `off`.
- **A crash between token computation and the deferred `RecordUsage` call is
  unrecorded** (the same window that already exists for the stderr request
  log right next to it) — accepted, this is analytics, not a ledger of
  record for billing reconciliation.
- **No new runtime dependency.** The stdlib + `golang.org/x/...` policy in
  docs/spec.md holds; valyrium ships zero third-party modules.
