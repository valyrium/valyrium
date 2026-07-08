# llm-gateway (Go implementation)

A zero-dependency OpenAI-compatible HTTP gateway that routes requests to the Claude Code CLI (`claude -p`). Any tool built against the OpenAI Chat Completions API can use Claude models with this gateway.

## Build and Run

### Build

```bash
go build -o bin/llm-gateway ./cmd/llm-gateway
```

### Run

```bash
./bin/llm-gateway
# or with custom config:
CLAUDE_GATEWAY_PORT=8787 CLAUDE_GATEWAY_MODEL=opus ./bin/llm-gateway
```

The server starts with:
```
llm-gateway listening on http://127.0.0.1:8787 (default model: sonnet, concurrency: 4, auth: open)
```

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
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | If set, all routes except `/healthz` require this key as `Authorization: Bearer <key>` or `x-api-key: <key>` |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model; also fallback for unrecognized ids |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models` and accepted as valid request models |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to Claude Code CLI executable |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Per-request wall-clock limit on the CLI process (300 seconds) |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Maximum simultaneous CLI processes; excess requests queue FIFO |

## HTTP API

### `GET /healthz`
Liveness probe (no auth required). Returns `{"ok":true}`.

### `GET /v1/models`
List configured models. Requires auth if `CLAUDE_GATEWAY_API_KEY` is set.

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

### `GET /metrics`
Prometheus-format metrics (requires auth if key is set). Tracks:
- `llmgateway_requests_total` — counter of requests by path and status
- `llmgateway_inflight_requests` — gauge of in-flight requests
- `llmgateway_request_duration_seconds` — summary of request latency

## Structured Logging

After each `/v1/chat/completions` request, a JSON line is written to stderr:
```json
{"method":"POST","path":"/v1/chat/completions","model":"sonnet","status":200,"duration_ms":102,"prompt_tokens":10,"completion_tokens":2}
```

This allows easy integration with log aggregation systems.

## Graceful Shutdown

The server listens for `SIGINT` and `SIGTERM`. On signal, in-flight requests are allowed to complete (up to the per-request timeout), then the process exits.

## Implementation

- **cmd/llm-gateway/main.go** — entry point, reads config from env, starts server
- **internal/gateway/server.go** — HTTP routing, auth, concurrency semaphore, streaming, metrics
- **internal/gateway/openai.go** — OpenAI wire format, prompt flattening, usage mapping
- **internal/gateway/claudecli.go** — subprocess runner, stream-json parsing, lifecycle
- **internal/gateway/metrics.go** — Prometheus exposition format writer
- **go.mod** — no runtime dependencies (stdlib only)

## Differences from TypeScript reference

The Go implementation maintains API parity with the TypeScript version (see `src/llm-gateway/`) plus these enhancements:

1. **cost_usd exposed** — the non-streaming response's `usage` and streaming terminal chunk's `usage` both include `cost_usd` from the CLI
2. **/metrics endpoint** — Prometheus-format metrics (requests_total, inflight_requests, request_duration_seconds)
3. **Structured stderr logging** — JSON request logs after each completion
4. **Constant-time auth** — API key comparison uses `crypto/subtle.ConstantTimeCompare`
5. **Graceful shutdown** — SIGINT/SIGTERM handling allows in-flight requests to finish

## Testing

```bash
go test ./... -v
go test -run TestAPIParity -v ./...
go test -run TestEnhancements -v ./...
```

Unit tests cover prompt flattening, model resolution, finish-reason mapping, and usage calculation. Integration tests (TestAPIParity, TestEnhancements) start the server against a stub CLI script and verify all endpoints and enhancements.
