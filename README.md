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
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | If set, all routes except `/healthz` require this key as `Authorization: Bearer <key>` or `x-api-key: <key>` (compared in constant time) |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model; also fallback for unrecognized ids |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models` and accepted as valid request models |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to Claude Code CLI executable |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Wall-clock limit on the CLI process; per-turn for tool-calling sessions |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Maximum simultaneous *actively generating* CLI processes; excess requests queue FIFO. Parked tool sessions release their slot |
| `CLAUDE_GATEWAY_MAX_SESSIONS` | `16` | Maximum live tool-calling sessions (active + parked CLI processes); at the cap, new tool-carrying requests get `429` |
| `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` | `120000` | Maximum time a paused tool call waits for the client's result before the session is reaped |
| `CLAUDE_GATEWAY_SESSION_IDLE_MS` | `600000` | Idle threshold after which a session with no pending tool calls is reaped |
| `CLAUDE_GATEWAY_CONTEXT_LENGTH` | *(unset)* | Context window reported in `GET /v1/models`. Either a bare integer used as the fallback for ids that don't match a known Claude family (`sonnet`/`opus`/`haiku` default to `200000`), or a comma-separated `id=length` list for per-model overrides, e.g. `opus=1000000,my-proxy=32000` |

## HTTP API

### `GET /healthz`
Liveness probe (no auth required). Returns `{"ok":true}`.

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

Response includes `usage` object with token counts and `cost_usd` (from CLI's accounting):
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
