# llm-gateway — Reverse-Engineered Specification

## 1. Purpose

`llm-gateway` is a single-process HTTP service that exposes a subset of the
**OpenAI Chat Completions API** and fulfills every request by spawning the
**Claude Code CLI** (`claude -p`). It exists so that any client already built
against the OpenAI protocol — the `openai` SDKs, Open WebUI, LiteLLM,
LangChain, plain `curl` — can talk to Claude models using whatever
authentication the local `claude` binary is logged in with (OAuth subscription
login or `ANTHROPIC_API_KEY`), without that client knowing anything about
Anthropic's API or the CLI.

```
OpenAI client ──HTTP──▶ llm-gateway ──spawn──▶ claude -p --output-format stream-json ──▶ Claude
```

Design constraints that shape everything below:

- **Zero runtime dependencies.** Only `node:http`, `node:child_process`,
  `node:readline`, and a `claude` binary on `PATH`. Runs under Bun
  (`bun src/llm-gateway/server.ts`) or Node with type stripping.
- **Stateless per request.** Each completion is one fresh CLI process; no
  sessions, no shared state beyond a concurrency semaphore.
- **The CLI is used as a model endpoint, not an agent.** Every spawn disables
  tools, session persistence, and slash commands so nothing agentic can
  execute on the gateway host.

## 2. Source layout

| File | Lines | Responsibility |
|---|---|---|
| `src/llm-gateway/server.ts` | ~260 | HTTP server: routing, auth, config, concurrency, SSE framing, error mapping |
| `src/llm-gateway/claude-cli.ts` | ~205 | Process runner: spawns `claude -p`, parses its `stream-json` output, enforces timeout/abort |
| `src/llm-gateway/openai-compat.ts` | ~160 | Pure translation layer: OpenAI messages → CLI prompt; CLI result → OpenAI response/chunk shapes |
| `src/llm-gateway/README.md` | — | User-facing docs (run instructions, config table, caveats) |

Dependency direction: `server.ts` → { `claude-cli.ts`, `openai-compat.ts` };
`openai-compat.ts` imports only types from `claude-cli.ts`. There are no other
modules and no test files.

## 3. Configuration

All configuration is via environment variables, read once at startup into a
`config` object (`server.ts:39`):

| Env var | Default | Meaning |
|---|---|---|
| `CLAUDE_GATEWAY_PORT` | `8787` | Listen port |
| `CLAUDE_GATEWAY_HOST` | `127.0.0.1` | Bind address (loopback-only by default) |
| `CLAUDE_GATEWAY_API_KEY` | *(unset)* | If set, every route except `/healthz` requires it as `Authorization: Bearer <key>` or `x-api-key: <key>` |
| `CLAUDE_GATEWAY_MODEL` | `sonnet` | Default model; also the fallback for unrecognized model ids |
| `CLAUDE_GATEWAY_MODELS` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models`; also accepted as valid request models |
| `CLAUDE_GATEWAY_BIN` | `claude` | Path to the Claude Code CLI executable |
| `CLAUDE_GATEWAY_TIMEOUT_MS` | `300000` | Per-request wall-clock limit on the CLI process |
| `CLAUDE_GATEWAY_CONCURRENCY` | `4` | Maximum simultaneous CLI processes; excess requests queue FIFO |

## 4. HTTP surface

### 4.1 Routing and auth

A single `createServer` handler (`server.ts:221`) dispatches on method + path:

1. `GET /healthz` → `200 {"ok":true}` — checked **before** auth, so it always
   works as a liveness probe.
2. Auth gate: if `CLAUDE_GATEWAY_API_KEY` is set and neither header matches,
   → `401` with `type: "authentication_error"`.
3. `GET /v1/models` → OpenAI-shaped model list: each id from
   `CLAUDE_GATEWAY_MODELS` as `{id, object:"model", created:0, owned_by:"anthropic"}`.
4. `POST /v1/chat/completions` → the completion flow (§5).
5. Anything else → `404` with `type: "not_found_error"`.

All error bodies use the OpenAI error envelope:
`{"error": {"message", "type", "code": null, "param": null}}`.

### 4.2 Accepted request body (`ChatCompletionRequest`)

```jsonc
{
  "model": "sonnet",              // optional; see model resolution (§5.2)
  "messages": [...],              // required, non-empty; roles: system|developer|user|assistant
  "stream": true,                 // optional; SSE when true
  "reasoning_effort": "high",     // optional; low|medium|high|xhigh|max → CLI --effort
  "temperature": 1,               // accepted, silently ignored (CLI has no knob)
  "top_p": 1,                     // accepted, silently ignored
  "max_tokens": 1024,             // accepted, silently ignored
  "max_completion_tokens": 1024   // accepted, silently ignored
}
```

Request bodies over **32 MiB** are rejected with `400 request body too large`
(`server.ts:101`).

## 5. Chat completion flow (`handleChatCompletions`)

### 5.1 Message → prompt translation (`openai-compat.ts: buildPrompt`)

The CLI is stateless and takes one prompt string, so OpenAI's message array is
flattened:

- **`system` and `developer`** messages are concatenated (joined with blank
  lines) into the CLI's `--system-prompt`. If none are present, the default is
  `"You are a helpful assistant."` This **replaces** Claude Code's own agent
  system prompt, so responses behave like a plain model call.
- **`user` / `assistant`** messages become conversation turns.
- **`tool`** (or any other) role → `400 unsupported message role`.
- Content may be a string or an array of `{type:"text", text}` parts; parts
  are joined. Any non-text part (images, audio) → `400 only text content
  parts are supported`. `null` content is treated as `""`.

Validation: the turn list must be non-empty and **end with a user message**,
otherwise `400 the last non-system message must be a user message`.

Two output shapes:

- **Single user turn** → the user text is passed through verbatim as the
  prompt.
- **Multi-turn history** → turns are serialized as a transcript
  (`[user]: …\n\n[assistant]: …`), and a fixed instruction is appended to the
  system prompt telling the model the user message contains a transcript and
  to write only the next assistant reply. This is the gateway's core trick for
  simulating multi-turn chat over a stateless CLI.

### 5.2 Model resolution (`server.ts: resolveModel`)

- Missing model → `CLAUDE_GATEWAY_MODEL`.
- Requested model passes through if it starts with `claude` (full ids like
  `claude-sonnet-5`) or case-insensitively matches a known alias:
  `sonnet | opus | haiku | fable` plus everything in `CLAUDE_GATEWAY_MODELS`.
- **Anything else falls back to the default model** rather than erroring —
  deliberately, so clients hard-coded to `gpt-4o` etc. work unmodified.

`reasoning_effort` is forwarded as `--effort` only if it is one of
`low | medium | high | xhigh | max`; invalid values are silently dropped.

### 5.3 Concurrency and cancellation

- A FIFO **semaphore** (`server.ts:51`) caps simultaneous CLI processes at
  `CLAUDE_GATEWAY_CONCURRENCY` (minimum 1). Excess requests wait in arrival
  order; the slot is acquired *before* spawning and released in a `finally`.
- An `AbortController` is aborted when the client socket closes before the
  response ends (`server.ts:146`); the runner turns that into an immediate
  `SIGKILL` of the CLI process and a `499`-coded error. Note the semaphore is
  acquired before the abort can matter, so a queued-then-cancelled request
  still briefly occupies a slot when it reaches the front.

### 5.4 Non-streaming response

On success: `200` with the OpenAI `chat.completion` object —
one choice, `message: {role:"assistant", content}`, `finish_reason`, `usage`
(see §7), `id` of the form `chatcmpl-<32-hex-uuid>`, `created` in epoch
seconds, and `model` set to the **actual model reported by the CLI** (not the
requested alias).

### 5.5 Streaming response (SSE)

When `stream: true`:

1. Headers are written immediately: `text/event-stream`, `cache-control:
   no-cache`, `x-accel-buffering: no` (disables nginx buffering).
2. First chunk: `delta: {role:"assistant", content:""}` (OpenAI role
   preamble).
3. Each CLI `text_delta` → one `chat.completion.chunk` with
   `delta: {content}` — deltas are forwarded live via the `onTextDelta`
   callback, not buffered.
4. Terminal chunk: empty delta, populated `finish_reason`, and a `usage`
   object (OpenAI-style usage-on-final-chunk).
5. `data: [DONE]\n\n`.

If the CLI fails **after** headers are out, the error cannot become an HTTP
status; it is emitted as a terminal SSE event
`data: {"error": {message, type:"api_error"}}` and the stream is closed
without `[DONE]` (`server.ts:186-192`).

### 5.6 Error → status mapping

| Condition | Status |
|---|---|
| Malformed JSON, bad messages, oversized body, non-text parts | `400 invalid_request_error` |
| Missing/wrong API key | `401 authentication_error` |
| Client disconnected mid-request | `499` (via `ClaudeCliError.statusCode`) |
| CLI spawn failure (binary not found) | `500 api_error` |
| CLI reported `is_error` result or exited without a result | `502 api_error` |
| CLI exceeded `CLAUDE_GATEWAY_TIMEOUT_MS` | `504 api_error` |
| `ClaudeCliError` with out-of-range code | coerced to `502` |

## 6. CLI runner (`claude-cli.ts: runClaude`)

### 6.1 Spawn contract

Each call spawns exactly one process:

```
<claudeBin> -p \
  --output-format stream-json --include-partial-messages --verbose \
  --tools "" --no-session-persistence --disable-slash-commands \
  --system-prompt <systemPrompt> \
  [--model <model>] [--effort <effort>]
```

- `--tools ""` + `--no-session-persistence` + `--disable-slash-commands`
  strip all agentic capability: the process is a pure text-in/text-out model
  call and leaves no session files behind.
- The **prompt is written to stdin**, not argv, so long transcripts never hit
  OS argument-length limits (`claude-cli.ts:202`). The system prompt, by
  contrast, does travel via argv.
- `--include-partial-messages --verbose` make the CLI emit incremental
  `stream_event` lines, which is what enables SSE token streaming.

### 6.2 Output parsing

stdout is read line-by-line (`node:readline`); each line is parsed as JSON
and non-JSON lines are ignored. Three line types matter:

| Line | Handling |
|---|---|
| `type:"stream_event"`, `content_block_delta` / `text_delta` | Text delta: appended to `streamedText`, forwarded to `onTextDelta` |
| `type:"stream_event"`, `message_delta` with `stop_reason` | Records stop reason |
| `type:"assistant"` with `message.model` | Records the real model id used |
| `type:"result"` | Authoritative terminal record: final text, `stop_reason`, `total_cost_usd`, token usage. `is_error:true` → rejection with the CLI's error detail |

The final `text` is the `result` line's text when present, falling back to
the concatenated stream deltas. stderr is retained as a rolling **2000-char
tail** and used as the error detail if the process exits without ever
emitting a `result` line.

### 6.3 Lifecycle guarantees

A single `finish()` gate (`claude-cli.ts:109`) ensures exactly-once
settlement and, on every path (success, error, timeout, abort):

- clears the timeout timer,
- removes the abort listener,
- **SIGKILLs the child if still running** — so timeouts and client
  disconnects never leak CLI processes.

Settlement triggers: abort signal (`499`), timeout (`504`), spawn error
(`500`), CLI `is_error` result (`502`), process close with a result seen
(success) or without one (`502`).

### 6.4 Runner result (`ClaudeCompletion`)

`{ text, model, stopReason, usage: {inputTokens, outputTokens,
cacheReadInputTokens, cacheCreationInputTokens}, costUsd }` — `costUsd` is
captured from the CLI but **not exposed** in the OpenAI response shape.

## 7. Usage and finish-reason mapping (`openai-compat.ts`)

- `prompt_tokens` = `input_tokens + cache_read_input_tokens +
  cache_creation_input_tokens` — OpenAI clients expect one input number;
  Claude splits it across cache buckets, so they are summed.
- `completion_tokens` = `output_tokens`; `total_tokens` is the sum.
- Stop reason mapping: `max_tokens` → `length`, `refusal` →
  `content_filter`, everything else (including `null` and `end_turn`) →
  `stop`.

## 8. Security model

- **Binding**: loopback-only by default; exposure requires an explicit
  `CLAUDE_GATEWAY_HOST=0.0.0.0`.
- **Auth**: optional single shared key, compared with `===` (no constant-time
  comparison); `/healthz` is intentionally unauthenticated.
- **Agentic surface**: disabled per spawn (§6.1), so a hostile prompt cannot
  run tools, read files, or execute commands on the host.
- **Resource abuse**: bounded by the 32 MiB body cap, the concurrency
  semaphore, and the per-request timeout.
- **Trust boundary caveat**: the system prompt is passed via argv, which is
  visible in the host's process listing (`ps`) while a request is running.

## 9. Known limitations (by design)

- **Cold-start latency**: every request pays CLI process startup (~1–2 s)
  before the first token.
- **Text only**: image/audio content parts → `400`.
- **No tool/function calling passthrough**: `tools` in the request is not
  translated (it isn't rejected either — simply ignored).
- **No sampling controls**: `temperature`, `top_p`, `max_tokens` are accepted
  and ignored; the CLI does not expose them.
- **Multi-turn fidelity**: history is a flattened text transcript, not real
  message-array turns — the model sees `[user]:`/`[assistant]:` markers, so
  behavior can diverge subtly from a native multi-turn API call.
- **Terms of use**: with consumer OAuth auth, Anthropic's consumer terms
  govern usage — a personal/dev convenience, not a resale mechanism. With
  `ANTHROPIC_API_KEY` it is just another API client.

## 10. Observability

Startup logs one line (host, port, default model, concurrency, auth mode).
There is no per-request logging, no metrics endpoint, and no persistence of
any kind. `total_cost_usd` from the CLI is parsed but currently dropped —
the one obvious hook for future cost accounting.

## 11. Go implementation

A full Go port of the llm-gateway exists at `cmd/llm-gateway` and `internal/gateway`.
It maintains behavioral parity with the TypeScript reference while using the
standard library only (zero runtime dependencies, `go.mod` contains no `require` lines).

### Intentional behavioral differences from TypeScript

1. **cost_usd exposed**: The non-streaming response's `usage` object and streaming
   terminal chunk's `usage` both carry `cost_usd` from the CLI (previously parsed
   but dropped in the TypeScript version).

2. **/metrics endpoint**: `GET /metrics` returns Prometheus exposition format with
   standard counters/gauges/summaries:
   - `llmgateway_requests_total{path,status}` counter
   - `llmgateway_inflight_requests` gauge
   - `llmgateway_request_duration_seconds` summary

3. **Per-request structured logging**: After each POST /v1/chat/completions, a
   JSON line is written to stderr with method, path, model, status, duration_ms,
   prompt_tokens, completion_tokens.

4. **Constant-time auth**: API key comparison uses `crypto/subtle.ConstantTimeCompare`
   instead of `===`.

5. **Graceful shutdown**: SIGINT/SIGTERM are caught; in-flight requests are
   allowed to complete before exit.
