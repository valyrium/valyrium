# valyrium — Specification

This document specifies the Go implementation of valyrium (`cmd/valyrium`, `internal/gateway`) — the only implementation in this repository.

## 1. Purpose

`valyrium` is a single-process Go binary that exposes a subset of the
**OpenAI Chat Completions API** and fulfills every request by spawning the
**Claude Code CLI** (`claude -p`). It exists so that any client already built
against the OpenAI protocol — the `openai` SDKs, Open WebUI, LiteLLM,
LangChain, plain `curl` — can talk to Claude models using whatever
authentication the local `claude` binary is logged in with (OAuth subscription
login or `ANTHROPIC_API_KEY`), without that client knowing anything about
Anthropic's API or the CLI.

```
OpenAI client ──HTTP──▶ valyrium ──spawn──▶ claude -p --output-format stream-json ──▶ Claude
```

Design constraints that shape everything below:

- **Minimal runtime dependencies.** Go standard library plus `golang.org/x/...`
  packages only — no third-party modules from outside the Go project. This is
  checked against `go list -m all`, which reports what every dependency's
  `go.mod` *declares*, not just what gets compiled: a module that drags its own
  test and CLI dependencies into the graph violates the policy even if none of
  their code ships. `go.etcd.io/bbolt` was briefly authorized for the persisted
  usage store and then withdrawn for exactly that reason
  (docs/adr/0004-usage-persistence.md).
- **Stateless per request, by default.** Each completion is one fresh CLI
  process; no sessions, no shared state beyond a concurrency semaphore.
  Requests carrying `tools` are the one exception (§9): they hold a session
  open across an HTTP round trip so the client can execute the proposed
  tool call and hand the result back.
- **The CLI is used as a model endpoint, not an agent.** Every spawn disables
  built-in tools, session persistence, and slash commands so nothing agentic
  can execute on the gateway host — the relay in §9 only ever carries tool
  proposals out and results back in, it never executes anything itself.

## 2. Source layout

| File | Responsibility |
|---|---|
| `cmd/valyrium/main.go` | Entry point: reads config from env, starts the server |
| `internal/gateway/server.go` | HTTP routing, auth, concurrency semaphore, SSE framing, tool-turn driving, graceful shutdown |
| `internal/gateway/openai.go` | OpenAI wire types, message-to-prompt flattening (incl. cold tool history), finish-reason and usage mapping |
| `internal/gateway/claudecli.go` | Subprocess runner: spawns `claude -p`, parses its `stream-json` output, enforces timeout/abort, session-mode spawning |
| `internal/gateway/session.go` | Tool-calling session table: pending-call correlation, idle/timeout sweeper, `MAX_SESSIONS` accounting |
| `internal/gateway/mcpserver.go` | The `/mcp/{sessionId}` JSON-RPC surface (`initialize`, `tools/list`, `tools/call`) |
| `internal/gateway/metrics.go` | Prometheus exposition format writer |
| `go.mod` | Stdlib plus `golang.org/x/...` — no third-party modules |

## 3. Configuration

All configuration is via environment variables, read once at startup into a
`Config` struct (`internal/gateway/server.go`):

| Env var | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_PORT` | `8787` | Listen port |
| `CLAUDE_GATEWAY_HOST` | `127.0.0.1` | Bind address (loopback-only by default) |
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | If set, every route except `/healthz` requires it as `Authorization: Bearer <key>` or `x-api-key: <key>` |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model; also the fallback for unrecognized model ids |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models`; also accepted as valid request models |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to the Claude Code CLI executable |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Wall-clock limit on the CLI process; per-turn for tool-calling sessions |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Maximum simultaneous *actively generating* CLI processes; excess requests queue FIFO. Parked tool sessions release their slot |
| `CLAUDE_GATEWAY_MAX_SESSIONS` | `16` | Maximum live tool-calling sessions (active + parked CLI processes); at the cap, new tool-carrying requests get `429` |
| `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` | `120000` | Maximum time a paused tool call waits for the client's result before the session is reaped |
| `CLAUDE_GATEWAY_SESSION_IDLE_MS` | `600000` | Idle threshold after which a session with no pending tool calls is reaped |
| `CLAUDE_GATEWAY_EXPOSE_REASONING` | `false` | If `true`, thinking blocks from the CLI stream are relayed as `reasoning_content` (on the message and on streaming deltas) instead of being dropped |

## 4. HTTP surface

### 4.1 Routing and auth

`Server.ServeHTTP` dispatches on method + path:

1. `GET /healthz` → `200 {"ok":true}` — checked **before** auth, so it always
   works as a liveness probe.
2. `POST /mcp/{sessionId}` → the MCP relay endpoint (§9); also checked before
   the API-key gate, since the spawned CLI does not carry the gateway's key.
3. Auth gate: if `CLAUDE_GATEWAY_API_KEY` is set and neither header matches,
   → `401` with `type: "authentication_error"`.
4. `GET /v1/models` → OpenAI-shaped model list: each id from
   `CLAUDE_GATEWAY_MODELS` as `{id, object:"model", created:0, owned_by:"anthropic"}`.
5. `POST /v1/chat/completions` → the completion flow (§5).
6. `GET /metrics` → Prometheus exposition format (§8).
7. Anything else → `404` with `type: "not_found_error"`.

All error bodies use the OpenAI error envelope:
`{"error": {"message", "type", "code": null, "param": null}}`.

### 4.2 Accepted request body (`ChatCompletionRequest`)

```jsonc
{
  "model": "sonnet",              // optional; see model resolution (§5.2)
  "messages": [...],              // required, non-empty; roles: system|developer|user|assistant|tool
  "tools": [...],                 // optional; see §9
  "tool_choice": "auto",          // optional; only the string "none" changes behavior
  "stream": true,                 // optional; SSE when true
  "reasoning_effort": "high",     // optional; low|medium|high|xhigh|max → CLI --effort
  "temperature": 1,               // accepted, silently ignored (CLI has no knob)
  "top_p": 1,                     // accepted, silently ignored
  "max_tokens": 1024,             // accepted, silently ignored
  "max_completion_tokens": 1024   // accepted, silently ignored
}
```

Request bodies over **32 MiB** are rejected with `400 request body too large`.

## 5. Chat completion flow (`handleChatCompletions`)

### 5.1 Message → prompt translation (`openai.go: BuildPrompt`)

The CLI is stateless and takes one prompt string, so OpenAI's message array is
flattened:

- **`system` and `developer`** messages are concatenated (joined with blank
  lines) into the CLI's `--system-prompt`. If none are present, the default is
  `"You are a helpful assistant."` This **replaces** Claude Code's own agent
  system prompt, so responses behave like a plain model call.
- **`user` / `assistant`** messages become conversation turns.
- **`tool`** messages and assistant messages carrying `tool_calls` are
  accepted and flattened into the transcript as
  `[assistant called tool <name> (<id>)]: <arguments>` /
  `[tool <name> (<id>) returned]: <content>` markers (§9.4) — this is the
  cold-history path, used when the tool exchange does not correlate to a
  live session.
- Content may be a string or an array of `{type:"text", text}` parts; parts
  are joined. Any non-text part (images, audio) → `400 only text content
  parts are supported`. `null` content is treated as `""`.

Validation: the turn list must be non-empty and **end with a user or tool
message**, otherwise `400 the last non-system message must be a user message`.

Two output shapes:

- **Single user turn** → the user text is passed through verbatim as the
  prompt.
- **Multi-turn history** → turns are serialized as a transcript
  (`[user]: …\n\n[assistant]: …`), and a fixed instruction is appended to the
  system prompt telling the model the user message contains a transcript and
  to write only the next assistant reply. This is the gateway's core trick for
  simulating multi-turn chat over a stateless CLI.

### 5.2 Model resolution (`server.go: resolveModel`)

- Missing model → `CLAUDE_GATEWAY_MODEL`.
- Requested model passes through if it starts with `claude` (full ids like
  `claude-sonnet-5`) or case-insensitively matches a known alias:
  `sonnet | opus | haiku | fable` plus everything in `CLAUDE_GATEWAY_MODELS`.
- **Anything else falls back to the default model** rather than erroring —
  deliberately, so clients hard-coded to `gpt-4o` etc. work unmodified.

`reasoning_effort` is forwarded as `--effort` only if it is one of
`low | medium | high | xhigh | max`; invalid values are silently dropped.

### 5.3 Concurrency and cancellation

- A FIFO **semaphore** (`server.go: Semaphore`) caps simultaneous CLI
  processes at `CLAUDE_GATEWAY_CONCURRENCY` (minimum 1). Excess requests wait
  in arrival order; the slot is acquired *before* spawning and released in a
  deferred call. Parked tool sessions (§9) release their slot as soon as they
  respond with `tool_calls`, so they never hold concurrency capacity while
  idle.
- The request context is canceled when the client socket closes before the
  response ends; the runner turns that into an immediate `SIGKILL` of the
  CLI process and a `499`-coded error. Note the semaphore is acquired before
  the cancellation can matter, so a queued-then-cancelled request still
  briefly occupies a slot when it reaches the front.

### 5.4 Non-streaming response

On success: `200` with the OpenAI `chat.completion` object —
one choice, `message: {role:"assistant", content}`, `finish_reason`, `usage`
(see §7), `id` of the form `chatcmpl-<32-hex>`, `created` in epoch
seconds, and `model` set to the **actual model reported by the CLI** (not the
requested alias).

### 5.5 Streaming response (SSE)

When `stream: true`:

1. Headers are written immediately: `text/event-stream`, `cache-control:
   no-cache`, `x-accel-buffering: no` (disables nginx buffering).
2. First chunk: `delta: {role:"assistant", content:""}` (OpenAI role
   preamble).
3. Each CLI `text_delta` → one `chat.completion.chunk` with
   `delta: {content}` — deltas are forwarded live, not buffered.
4. Terminal chunk: empty delta, populated `finish_reason`, and a `usage`
   object (OpenAI-style usage-on-final-chunk).
5. `data: [DONE]\n\n`.

If the CLI fails **after** headers are out, the error cannot become an HTTP
status; it is emitted as a terminal SSE event
`data: {"error": {message, type:"api_error"}}` and the stream is closed
without `[DONE]`.

### 5.6 Error → status mapping

| Condition | Status |
|---|---|
| Malformed JSON, bad messages, oversized body, non-text parts | `400 invalid_request_error` |
| Missing/wrong API key | `401 authentication_error` |
| Client disconnected mid-request | `499` (via `ClaudeCliError.StatusCode`) |
| CLI spawn failure (binary not found) | `500 api_error` |
| CLI reported `is_error` result or exited without a result | `502 api_error` |
| CLI exceeded `CLAUDE_GATEWAY_TIMEOUT_MS` | `504 api_error` |
| Too many live tool sessions (§9) | `429 rate_limit_error` |
| `ClaudeCliError` with out-of-range code | coerced to `502` |

## 6. CLI runner (`claudecli.go: RunClaude` / `StartClaudeSession`)

### 6.1 Spawn contract

Each stateless call spawns exactly one process:

```
<claudeBin> -p \
  --output-format stream-json --include-partial-messages --verbose \
  --tools "" --no-session-persistence --disable-slash-commands \
  --system-prompt <systemPrompt> \
  [--model <model>] [--effort <effort>]
```

A tool-calling session (§9) adds `--strict-mcp-config --mcp-config
{"mcpServers":{"relay":{"type":"http","url":"<gateway's own /mcp/{sessionId}>"}}}`
and is not killed when its first turn ends — it stays alive, parked, until
the session resolves or is reaped.

- `--tools ""` + `--no-session-persistence` + `--disable-slash-commands`
  strip all agentic capability: the process is a pure text-in/text-out model
  call (or, with the MCP relay attached, a model call whose only tools are
  the ones the client's request declared) and leaves no session files
  behind.
- The **prompt is written to stdin**, not argv, so long transcripts never hit
  OS argument-length limits. The system prompt, by contrast, does travel via
  argv.
- `--include-partial-messages --verbose` make the CLI emit incremental
  `stream_event` lines, which is what enables SSE token streaming.

### 6.2 Output parsing

stdout is read line-by-line; each line is parsed as JSON and non-JSON lines
are ignored. Line types that matter:

| Line | Handling |
|---|---|
| `type:"stream_event"`, `content_block_delta` / `text_delta` | Text delta: appended to the running text, forwarded live |
| `type:"stream_event"`, `message_delta` with `stop_reason` | Records stop reason and, for tool turns, the per-turn usage |
| `type:"assistant"` with `message.content` containing `tool_use` blocks | Collected as proposed tool calls (§9) |
| `type:"assistant"` with `message.model` | Records the real model id used |
| `type:"result"` | Authoritative terminal record: final text, `stop_reason`, `total_cost_usd`, token usage. `is_error:true` → rejection with the CLI's error detail |

The final text is the `result` line's text when present, falling back to
the concatenated stream deltas. stderr is retained as a rolling tail and used
as the error detail if the process exits without ever emitting a `result`
line.

### 6.3 Lifecycle guarantees

A single settlement gate ensures exactly-once completion and, on every path
(success, error, timeout, abort):

- clears the timeout timer,
- removes the cancellation listener,
- **SIGKILLs the child if still running** — so timeouts and client
  disconnects never leak CLI processes. A parked tool session is the one
  process intentionally left running across an HTTP response; it is killed
  by the session sweeper instead (§9.5), not by the request that spawned it.

Settlement triggers: cancellation (`499`), timeout (`504`), spawn error
(`500`), CLI `is_error` result (`502`), process close with a result seen
(success) or without one (`502`).

### 6.4 Runner result

`{ text, model, stopReason, usage: {inputTokens, outputTokens,
cacheReadInputTokens, cacheCreationInputTokens}, costUSD }`.

## 7. Usage and finish-reason mapping (`openai.go`)

- `prompt_tokens` = `input_tokens + cache_read_input_tokens +
  cache_creation_input_tokens` — OpenAI clients expect one input number;
  Claude splits it across cache buckets, so they are summed.
- `completion_tokens` = `output_tokens`; `total_tokens` is the sum.
- `usage.cost_usd` carries the CLI's `total_cost_usd` on both the
  non-streaming response and the streaming terminal chunk.
- Stop reason mapping: `max_tokens` → `length`, `refusal` →
  `content_filter`, `tool_use` → `tool_calls`, everything else (including
  `null` and `end_turn`) → `stop`.

## 8. Observability

- **Structured request logs.** After each `POST /v1/chat/completions`, one
  JSON line is written to stderr: `method, path, model, status, duration_ms,
  prompt_tokens, completion_tokens`.
- **`GET /metrics`** returns Prometheus exposition format (stdlib only, no
  client library): `llmgateway_requests_total{path,status}` counter,
  `llmgateway_inflight_requests` gauge, `llmgateway_request_duration_seconds`
  summary, and `llmgateway_live_sessions` gauge (§9.5).
- Startup logs one line (host, port, default model, concurrency, auth mode).

## 9. Tool calling via MCP relay

The gateway supports OpenAI-style **tool calling** (`tools` + `tool_choice`
in the request, `finish_reason: "tool_calls"` in the response) even though
`claude -p` normally executes tools itself. Full design in
[ADR 0001](adr/0001-mcp-relay-tool-calling.md); summary below.

### 9.1 How it works

1. A request carrying `tools` (and `tool_choice` other than `"none"`) spawns
   the CLI pointed at the gateway's own `/mcp/{sessionId}` endpoint
   (`--strict-mcp-config --mcp-config ...`), registering the client's tool
   schemas as MCP tools for that session. Built-in tools stay disabled
   (`--tools ""`), so **nothing ever executes on the gateway host** — the
   relay only carries proposals out and results back in.
2. When the model proposes tool calls, the turn is finalized **from the
   CLI's stream output alone** (`stop_reason: "tool_use"`), never from MCP
   traffic: the response carries `choices[0].message.tool_calls`
   (gateway-minted `call_...` ids) and `finish_reason: "tool_calls"`. The CLI
   process stays alive, parked on its MCP request, at ~0 CPU.
3. The client executes the tools and sends the results back as
   `{"role":"tool","tool_call_id":...,"content":...}` messages. The gateway
   correlates them to the parked session **by tool_call_id**, resolves the
   paused MCP calls, and the CLI resumes — producing more text, another tool
   call, or the final answer.
4. Parallel and sequential tool dispatch are both supported; parallel
   conversations never cross-wire because correlation is by globally unique
   id, not content.

Streaming works too: text deltas stream as usual, tool calls arrive as a
`delta.tool_calls` chunk followed by a `finish_reason: "tool_calls"` terminal
chunk.

### 9.2 The `/mcp/{sessionId}` endpoint

Internal MCP relay endpoint used by the spawned CLI during tool-calling
sessions. Not for direct client use: the unguessable 128-bit session id is
the credential, so the endpoint bypasses the API-key gate. It implements
exactly three JSON-RPC 2.0 methods: `initialize`, `tools/list` (the request's
tool schemas, field-renamed to MCP's `{name, description, inputSchema}`), and
`tools/call`. Unknown session ids get a JSON-RPC error. JSON-RPC messages
without an `id` field are notifications and get `202` with an empty body.

### 9.3 Matching and correlation

A `tools/call` is matched to an announced `tool_use` by `(name,
canonicalized-JSON arguments)`, FIFO among identical pairs. Session
correlation on resume is by gateway-minted `tool_call_id` **only** — never by
matching message content, hashing prefixes, or comparing transcripts. A
`tools/call` naming a tool never announced on that session gets a JSON-RPC
error response instead of a park. A tool result can arrive before the CLI's
`tools/call` request for it (sequential dispatch); the session buffers
results keyed by `tool_call_id` and resolves a `tools/call` immediately if
its result is already buffered.

### 9.4 Cold history

Sessions are in-memory. If the gateway restarts, or a session is reaped
(§9.5), a continuation is not lost: tool history in the request is flattened
into the transcript as `[assistant called tool ...]` / `[tool ... returned]`
markers (§5.1, degraded fidelity) and a fresh session is started if the
request still carries `tools`.

`tool_choice: "none"` skips tool registration and takes the stateless path
(§5). Requests without `tools` are completely unaffected.

### 9.5 Session lifecycle and resource accounting

- `CLAUDE_GATEWAY_MAX_SESSIONS` (default 16) caps live tool-calling sessions
  (active + parked); at the cap, new tool-carrying requests get `429`.
  Requests without `tools` are never subject to this cap.
- `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` (default 120000) bounds how long a parked
  session waits for a client tool result before a background sweeper reaps
  it (SIGKILLs the CLI process).
- `CLAUDE_GATEWAY_SESSION_IDLE_MS` (default 600000) reaps a session with no
  pending tool calls after that much idle time (covers a CLI that lingers
  past its result line).
- Parked sessions release their concurrency slot (§5.3), so they never
  starve unrelated stateless requests.
- `llmgateway_live_sessions` on `/metrics` (§8) tracks the count.

## 10. Security model

- **Binding**: loopback-only by default; exposure requires an explicit
  `CLAUDE_GATEWAY_HOST=0.0.0.0`.
- **Auth**: optional single shared key, compared with
  `crypto/subtle.ConstantTimeCompare`; `/healthz` is intentionally
  unauthenticated, and `/mcp/{sessionId}` is authenticated by its unguessable
  session id instead of the API key (§9.2).
- **Agentic surface**: disabled per spawn (§6.1), so a hostile prompt cannot
  run tools, read files, or execute commands on the host — the MCP relay
  only ever proxies the *client's own declared* tool schemas, and never
  executes them itself.
- **Resource abuse**: bounded by the 32 MiB body cap, the concurrency
  semaphore, the per-request timeout, and the tool-session cap (§9.5).
- **Trust boundary caveat**: the system prompt is passed via argv, which is
  visible in the host's process listing (`ps`) while a request is running.

## 11. Known limitations (by design)

- **Cold-start latency**: every request pays CLI process startup (~1–2 s)
  before the first token.
- **Text only**: image/audio content parts → `400`.
- **No sampling controls**: `temperature`, `top_p`, `max_tokens` are accepted
  and ignored; the CLI does not expose them.
- **Multi-turn fidelity**: history is a flattened text transcript, not real
  message-array turns — the model sees `[user]:`/`[assistant]:` markers, so
  behavior can diverge subtly from a native multi-turn API call. The same
  applies to cold tool history (§9.4).
- **Terms of use**: with consumer OAuth auth, Anthropic's consumer terms
  govern usage — a personal/dev convenience, not a resale mechanism. With
  `ANTHROPIC_API_KEY` it is just another API client.
