<img src="docs/design/logo-mark.svg" width="56" height="56" alt="valyrium">

# valyrium

An OpenAI-compatible HTTP gateway that routes requests to the Claude Code CLI (`claude -p`). Any tool built against the OpenAI Chat Completions API can use Claude models with this gateway. Dependencies are limited to the Go standard library and `golang.org/x/...` packages, plus one named exception (`go.etcd.io/bbolt`, for persisted usage accounting) — no other third-party modules.

A built-in dashboard (`GET /dashboard`) is designed in docs/design/dashboard.html — see docs/adr/0003-embedded-dashboard.md and docs/adr/0004-usage-persistence.md.

## Install

```bash
brew install valyrium/tap/valyrium
```

## Build and Run

### Build

```bash
go build -o bin/valyrium ./cmd/valyrium
```

### Run

```bash
./bin/valyrium
# or with custom config:
CLAUDE_GATEWAY_PORT=8787 CLAUDE_GATEWAY_MODEL=opus ./bin/valyrium
```

The server starts with:
```
valyrium listening on http://127.0.0.1:8787 (default model: sonnet, concurrency: 4, auth: open)
```

`valyrium --version` (or `-v`) prints the build version and exits without starting the server.

Then use any OpenAI client:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"sonnet","messages":[{"role":"user","content":"Say hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="unused")
resp = client.chat.completions.create(
    model="sonnet",
    messages=[{"role": "user", "content": "Say hi"}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

## Environment Variables

All configuration is via environment variables, read at startup:

| Variable | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_PORT` | `8787` | HTTP listen port |
| `CLAUDE_GATEWAY_HOST` | `127.0.0.1` | Bind address (loopback-only by default) |
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | If set, all routes except `/healthz` and `/dashboard` require this key as `Authorization: Bearer <key>` or `x-api-key: <key>` (compared in constant time) |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model; also fallback for unrecognized ids |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models` and accepted as valid request models |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to Claude Code CLI executable |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Wall-clock limit on the CLI process; per-turn for tool-calling sessions |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Maximum simultaneous *actively generating* CLI processes; excess requests queue FIFO. Parked tool sessions release their slot |
| `CLAUDE_GATEWAY_MAX_SESSIONS` | `16` | Maximum live tool-calling sessions (active + parked CLI processes); at the cap, new tool-carrying requests get `429` |
| `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` | `120000` | Maximum time a paused tool call waits for the client's result before the session is reaped |
| `CLAUDE_GATEWAY_SESSION_IDLE_MS` | `600000` | Idle threshold after which a session with no pending tool calls is reaped |
| `CLAUDE_GATEWAY_CONTEXT_LENGTH` | *(unset)* | Context window reported in `GET /v1/models`. Either a bare integer used as the fallback for ids that don't match a known Claude family (`sonnet`/`opus`/`haiku` default to `200000`), or a comma-separated `id=length` list for per-model overrides, e.g. `opus=1000000,my-proxy=32000` |
| `CLAUDE_GATEWAY_RESUME` | `false` | Opt into cross-request conversation continuity (`1`/`true`/`yes`/`on`). See [Conversation continuity](#conversation-continuity-experimental) |
| `CLAUDE_GATEWAY_RESUME_MAX` | `32` | Maximum resumable conversations held in memory when `CLAUDE_GATEWAY_RESUME` is on |
| `CLAUDE_GATEWAY_EXPOSE_REASONING` | `false` | If `true`, thinking blocks from the CLI stream are relayed as `reasoning_content` (on the message and on streaming deltas) instead of being dropped |
| `CLAUDE_GATEWAY_USAGE_DB` | `$HOME/.valyrium/usage.db` | Path to the [bbolt](https://github.com/etcd-io/bbolt) file holding persisted token/cost totals. Set to `off` to disable usage tracking (no file, no usage gauges). If the file cannot be opened, the gateway logs a warning and runs with tracking disabled |

## HTTP API

### `GET /healthz`
Liveness probe (no auth required). Returns `{"ok":true}`.

### `GET /dashboard`
Human-facing dashboard: request volume by route and status, in-flight requests, live tool-calling sessions, configured models, and the token/cost ledger. Served unauthenticated ([ADR 0003](docs/adr/0003-embedded-dashboard.md)) because a browser navigation cannot set an auth header — the page is a static shell carrying no data, and its own `fetch()` calls to `/metrics` and `/v1/models` supply the key, which it prompts for on a `401` and caches in `localStorage`.

### `GET /v1/models`
List configured models. Requires auth if `CLAUDE_GATEWAY_API_KEY` is set. Each entry includes `context_length` and `max_model_len` (same value, two names) so OpenAI-compatible clients that auto-detect context windows don't fall back to a guessed default. See `CLAUDE_GATEWAY_CONTEXT_LENGTH` above to override.

### `POST /v1/chat/completions`
OpenAI Chat Completions endpoint. Supports both streaming (`stream:true`) and non-streaming.

Request body example:
```json
{
  "model": "sonnet",
  "messages": [
    {"role": "user", "content": "Say hi"}
  ],
  "stream": false,
  "reasoning_effort": "high"
}
```

#### Reasoning effort

The gateway maps reasoning effort to the CLI's `--effort` flag from either of two request shapes:

- Top-level `reasoning_effort: "low" | "medium" | "high" | "xhigh" | "max"` (OpenAI-style).
- An OpenRouter-style `reasoning` object: `{"reasoning": {"enabled": true, "effort": "medium"}}`.

If both are present, `reasoning_effort` wins. `{"reasoning": {"enabled": false}}` is ignored entirely — no `--effort` flag is passed, regardless of any `effort` value also present in the object — rather than being mapped to the lowest effort level.

Response includes `usage` object with token counts and `cost_usd` (from CLI's accounting). Cache-read tokens are broken out in `prompt_tokens_details.cached_tokens` (spec-standard) while still being folded into `prompt_tokens`; cache-write tokens are reported non-standard via `cache_write_tokens`:
```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "claude-sonnet-4-20250514",
  "choices": [...],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 2,
    "total_tokens": 12,
    "prompt_tokens_details": {
      "cached_tokens": 0
    },
    "cost_usd": 0.001
  }
}
```

#### `response_format` (structured output)

`response_format: {"type": "json_object"}` and `response_format: {"type": "json_schema", "json_schema": {"name": ..., "schema": {...}}}` are supported by prompt injection, not a native CLI mode:

1. The gateway appends an instruction to the system prompt telling the model to reply with only a JSON object (and, for `json_schema`, includes the schema verbatim in that instruction).
2. Non-streaming requests are validated: if the reply isn't parseable JSON (or isn't a JSON object, for `json_object`), the gateway retries once with the validation error appended to the prompt, then returns whichever attempt it has — the retry is best-effort, not a hard guarantee.
3. Streaming requests get the same prompt instruction but are not validated or retried (text is already in flight by the time an invalid reply would be detectable).
4. `json_schema` validation checks that the reply is valid JSON; it does not fully validate against the schema's constraints (required properties, types, etc.) beyond that.

### `POST /mcp/{sessionId}`
Internal MCP relay endpoint used by the spawned CLI during tool-calling sessions (see below). Not for direct client use: the unguessable 128-bit session id is the credential, so the endpoint bypasses the API-key gate. Unknown session ids get a JSON-RPC error.

### `GET /metrics`
Prometheus-format metrics (requires auth if key is set). Tracks:
- `llmgateway_requests_total` — counter of requests by path and status
- `llmgateway_inflight_requests` — gauge of in-flight requests
- `llmgateway_request_duration_seconds` — summary of request latency
- `llmgateway_live_sessions` — gauge of live tool-calling sessions (active + parked)
- `llmgateway_usage_input_tokens` / `llmgateway_usage_output_tokens` / `llmgateway_usage_cost_usd` — gauges of persisted usage, labelled `period="current|week|month|ytd|all"` ([ADR 0004](docs/adr/0004-usage-persistence.md)). Omitted entirely when usage tracking is off, so a scraper can tell "disabled" from "zero"

## Tool Calling (OpenAI functions) via MCP relay

The gateway supports OpenAI-style tool calling (`tools` + `tool_choice` in the request, `finish_reason: "tool_calls"` in the response) even though `claude -p` normally executes tools itself. Design: [ADR 0001](docs/adr/0001-mcp-relay-tool-calling.md).

How it works:

1. A request carrying `tools` spawns the CLI pointed at the gateway's own `/mcp/{sessionId}` endpoint (`--strict-mcp-config --mcp-config ...`), registering the client's tool schemas as MCP tools for that session. Built-in tools stay disabled (`--tools ""`), so **nothing ever executes on the gateway host** — the relay only carries proposals out and results back in.
2. When the model proposes tool calls, the turn is finalized from the CLI's stream output: the response carries `choices[0].message.tool_calls` (gateway-minted `call_...` ids) and `finish_reason: "tool_calls"`. The CLI process stays alive, parked on its MCP request, at ~0 CPU.
3. The client executes the tools and sends the results back as `{"role":"tool","tool_call_id":...,"content":...}` messages. The gateway correlates them to the parked session **by tool_call_id**, resolves the paused MCP calls, and the CLI resumes — producing more text, another tool call, or the final answer.
4. Parallel and sequential tool dispatch are both supported; parallel conversations never cross-wire because correlation is by globally unique id, not content.

Streaming works too: text deltas stream as usual, tool calls arrive as a `delta.tool_calls` chunk followed by a `finish_reason: "tool_calls"` terminal chunk.

Sessions are in-memory. If the gateway restarts (or a session is reaped by `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` / `CLAUDE_GATEWAY_SESSION_IDLE_MS`), a continuation is not lost: tool history in the request is flattened into the transcript as `[assistant called tool ...]` / `[tool ... returned]` markers (degraded fidelity, documented in the ADR) and a fresh session is started when the request carries `tools`.

`tool_choice: "none"` skips tool registration and takes the stateless path. Requests without `tools` are completely unaffected.

## Conversation continuity (experimental)

By default every conversational turn is stateless: the gateway spawns a fresh CLI process and flattens the whole history into a text transcript (`[user]:` / `[assistant]:` markers). That is faithful enough, but the history is re-sent cold on every turn, so cost and latency grow with conversation length.

Setting `CLAUDE_GATEWAY_RESUME=1` turns on continuity. The gateway fingerprints the conversation prefix (every message through the last assistant reply, plus the model and reasoning effort) and remembers which CLI session produced it. When the next turn arrives with that same prefix, the gateway resumes the CLI session (`--resume <id>`) and sends **only the new user message** — the real turn structure stays inside the CLI, and the provider can cache the prompt.

Anything that does not match falls back to today's flatten-and-replay, so a miss is only a cost, never an error:

- gateway restart (the map is in memory only)
- edited or trimmed history — a rewritten earlier turn changes the fingerprint
- a different model or `reasoning_effort` for the same history
- eviction from the LRU (`CLAUDE_GATEWAY_RESUME_MAX`, default 32 conversations)
- any request carrying tool history, which uses the MCP relay session mechanism above instead

Operational cost: resumed conversations leave CLI sessions persisted on disk (the flag drops `--no-session-persistence`), one per remembered conversation, bounded by `CLAUDE_GATEWAY_RESUME_MAX`. It stays off by default until it has been proven in production.

## Structured Logging

After each `/v1/chat/completions` request, a JSON line is written to stderr:
```json
{"method":"POST","path":"/v1/chat/completions","model":"sonnet","status":200,"duration_ms":102,"prompt_tokens":10,"completion_tokens":2}
```

This allows easy integration with log aggregation systems.

## Graceful Shutdown

The server listens for `SIGINT` and `SIGTERM`. On signal, in-flight requests are allowed to complete (up to the per-request timeout), then the process exits.

## Implementation

- **cmd/valyrium/main.go** — entry point, reads config from env, starts server
- **internal/gateway/server.go** — HTTP routing, auth, concurrency semaphore, streaming, metrics, tool-turn driving
- **internal/gateway/openai.go** — OpenAI wire format, prompt flattening (incl. cold tool history), usage mapping
- **internal/gateway/claudecli.go** — subprocess runner, stream-json parsing, lifecycle, session spawning
- **internal/gateway/session.go** — tool-calling session table, pending-call correlation, sweeper
- **internal/gateway/mcpserver.go** — the `/mcp/{sessionId}` JSON-RPC surface (initialize, tools/list, tools/call)
- **internal/gateway/metrics.go** — Prometheus exposition format writer
- **go.mod** — stdlib plus `golang.org/x/...` only, no other third-party dependencies

## Testing

```bash
go test ./... -v
go test -run TestAPIParity -v ./...
go test -run TestEnhancements -v ./...
go test -run 'TestMCPRelay|TestSequentialDispatchNoDeadlock|TestParallelSessionsNoCrossWire|TestSessionLifecycle|TestColdHistoryFlattening' -v ./internal/gateway/
```

Unit tests cover prompt flattening, model resolution, finish-reason mapping, and usage calculation. Integration tests (TestAPIParity, TestEnhancements) start the server against a stub CLI script and verify all endpoints and enhancements. The relay tests re-exec the test binary as a stub CLI that speaks real MCP over HTTP back to the gateway, covering the two-turn tool round trip, sequential-dispatch deadlock resistance, parallel-session correlation, session lifecycle/reaping, and cold-history flattening.
