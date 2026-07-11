# ADR 0001: MCP relay for OpenAI-style tool calling

## Status

Accepted. Implemented in `internal/gateway` (session.go, mcpserver.go,
claudecli.go, server.go) with the phase-4 proof harness in
`internal/gateway/relay_test.go`. Revision 2 — adversarial design review
fixed a finalization deadlock, replaced prefix-hash session correlation
with tool_call_id correlation, and detached session lifetime from request
lifetime (see "Revision history" at the end).

## Context

`valyrium` wraps `claude -p` to expose an OpenAI-compatible
`/v1/chat/completions`. It currently spawns the CLI with `--tools ""`,
so it is a pure text-in/text-out endpoint — no client ever sees a tool
call, and nothing agentic runs on the gateway host (docs/spec.md §1, §8).

Two candidate consumers — OpenClaw and Hermes Agent — are themselves
autonomous agent frameworks, not chat UIs. Both *require* OpenAI-style
tool calling to function at all: the server proposes a tool call and
stops (`finish_reason: "tool_calls"`), the client executes it, and the
client sends the result back in a follow-up request. That contract is
the opposite of how `claude -p` works today: when the model wants a
tool, the CLI invokes it itself, in-process, and feeds the result back
to the model without ever surfacing the call to the gateway's caller.

This ADR designs a way to get OpenAI's server-proposes /
client-executes contract out of a CLI that only knows how to
execute-tools-itself, **without executing anything on the gateway
host** — preserving the one security property that mattered enough to
disable `--tools` in the first place.

## Alternatives considered

- **Bypass the CLI; call the Anthropic Messages API directly when
  `tools` is present.** Simpler and gets native `tool_use` semantics,
  but drops CLI/OAuth-subscription auth reuse on the agentic path.
  Not chosen; this ADR is the design for the relay option.
- **Kill the CLI after each turn and resume via session persistence
  (`claude -p --resume <id>`, dropping `--no-session-persistence`).**
  Attractive because no idle processes survive between turns and
  sessions survive a gateway restart. Rejected: there is no clean way
  to inject a *tool result* into a resumed session — `--resume` takes
  a new user prompt, but the transcript ends with an unresolved
  `tool_use` block, and faking the result as user text corrupts the
  turn structure. Revisit only if the CLI grows a first-class
  resume-with-tool-result input.

## Decision

Give the gateway itself an MCP server surface. When a request carries
`tools`, spawn the CLI pointed at that surface, register the client's
tool schemas as MCP tools for the lifetime of that conversation, and
turn every `tools/call` the CLI makes into a **pause**: hand the
proposed call back to the original HTTP caller as `tool_calls` +
`finish_reason: "tool_calls"`, and keep the (idle) CLI process alive
until a follow-up request supplies the result.

The gateway becomes both the OpenAI-facing HTTP server and the CLI's
MCP server, in one process. No second binary, no subprocess besides
the CLI itself.

```
OpenAI client                    valyrium                         claude -p (per session)
     │  POST /v1/chat/completions     │                                   │
     │  tools:[...]                   │                                   │
     ├────────────────────────────────►  spawn with                      │
     │                                │  --mcp-config http://127.0.0.1/  │
     │                                │    mcp/<session>                 │
     │                                │  --strict-mcp-config --tools ""  │
     │                                ├──────────────────────────────────►
     │                                │                                   │  tools/list → session's tool schemas
     │                                │  ◄─ stream-json: tool_use blocks ┤  (model announces its calls)
     │                                │     + stop_reason "tool_use"     │
     │  ◄── 200 tool_calls, ──────────┤  finalize from the STREAM,       │
     │      finish_reason=tool_calls  │  not from MCP traffic (§4)       │
     │                                │  ◄─ tools/call {name,args} ───────┤  (parked; CLI blocks)
     │  (client executes the tool)    │                                   │
     │  POST /v1/chat/completions     │                                   │
     │  messages: [...,               │                                   │
     │   {role:"tool", tool_call_id,  │                                   │
     │    content:<result>}]          │                                   │
     ├────────────────────────────────►  looks up session BY             │
     │                                │  tool_call_id (§6), resolves ────►│  tools/call responds, CLI resumes
     │                                │  the parked call                  │  (more text, or the next tool call)
     │  ◄── 200 normal completion ────┤ ◄─ stream-json deltas ────────────┤
```

## Mechanism

### 1. MCP endpoint lives in the gateway

Add `POST /mcp/{sessionId}` (Streamable HTTP; confirm the transport
version the installed `claude` build speaks at implementation time)
implementing exactly three JSON-RPC methods:

- `initialize` — standard handshake.
- `tools/list` — returns the tool schemas registered for `sessionId`.
  OpenAI's `{type:"function", function:{name, description, parameters}}`
  maps to MCP's `{name, description, inputSchema}` by field rename;
  both use JSON Schema for parameters, so no semantic translation.
- `tools/call` — the pause point (§5).

`sessionId` is 128 bits from a CSPRNG (`crypto/rand`); it is the only
credential guarding the endpoint, so it must be unguessable. Unknown
session ids get a JSON-RPC error, never a fallback. Note for
operators: with `CLAUDE_GATEWAY_HOST=0.0.0.0` the MCP endpoint is
network-reachable too; the session id is the sole barrier. If the
mcp-config `headers` field is supported by the installed CLI, add a
per-session bearer token as defense in depth.

### 2. Spawning a tool-capable session

When `body.tools` is present (and `tool_choice` is not `"none"` —
that case just omits tool registration and takes the stateless path),
the gateway:

1. Mints a `sessionId`, registers the translated tools under it.
2. Spawns the CLI with the current arg set **plus**
   `--strict-mcp-config --mcp-config '{"mcpServers":{"relay":{"type":"http","url":"http://127.0.0.1:<port>/mcp/<sessionId>"}}}'`.
   `--tools ""` stays: built-in tools remain disabled;
   `--strict-mcp-config` ignores any project/global `.mcp.json`, so
   the model can reach *only* the tools this request supplied — never
   the host's own MCP servers.
3. Sets `MCP_TOOL_TIMEOUT` (and `MCP_TIMEOUT`) in the child's
   environment to a value comfortably above
   `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS`, so the CLI's own MCP timeout
   cannot kill a parked call before the gateway's does. (Env var names
   as documented for Claude Code; verify against the installed build.)
4. Keeps the process handle alive past the first response, owned by
   the session table — **not** by the HTTP request (§7).

Requests with no `tools` are completely unaffected: same one-shot
stateless path as today — no session, no MCP registration, no new
state.

### 3. Tool-aware prompt flattening (cold history)

A request may arrive carrying tool history the gateway has no live
session for: the gateway restarted, the session was reaped, or the
client is replaying a transcript that began elsewhere. Rather than
erroring, extend `buildPrompt`'s multi-turn flattening to serialize
tool exchanges into the transcript:

```
[assistant]: {text, if any}
[assistant called tool get_weather (call_abc123)]: {"city":"Nairobi"}
[tool get_weather (call_abc123) returned]: {"temp_c":24}
```

with one added line in the transcript instruction explaining the
markers. This is degraded fidelity (the model sees prose markers, not
native tool_use blocks) but it makes gateway restarts and reaped
sessions *recoverable* instead of conversation-fatal. A fresh session
is started for the continuation if the request also carries `tools`.

### 4. Finalizing a turn: the stream is the source of truth, never MCP traffic

Anthropic delivers every `tool_use` content block for a turn in the
assistant message itself, and the CLI's `stream-json` output already
carries them (the same `assistant` / `stream_event` lines the runner
parses today), ending with `stop_reason: "tool_use"`.

**The client-facing `tool_calls` response is finalized entirely from
that stream**: when the turn ends with `stop_reason: "tool_use"`, mint
one OpenAI `tool_call_id` (`call_<random>`) per announced `tool_use`
block, emit `choices[0].message.tool_calls` (plus any text content the
model produced alongside), `finish_reason: "tool_calls"`, and return.

Do **not** wait for the corresponding MCP `tools/call` requests to
arrive before responding. This ordering is load-bearing: if the CLI
dispatches tool calls *sequentially* (issue call 2 only after call 1's
MCP response), a design that waits for all N `tools/call`s before
responding deadlocks — call 1 is parked awaiting a client result the
client can never send because the HTTP response hasn't been sent.
Finalizing from the stream announcement is correct under both
sequential and concurrent CLI dispatch, and it also gets parallel tool
calls (OpenAI allows several per turn) for free.

Streaming variant: forward Anthropic's `input_json_delta` fragments
for each `tool_use` block as OpenAI `delta.tool_calls[i].function.arguments`
fragments — the two formats stream partial-JSON arguments the same
way. Each streamed `tool_calls` entry carries its `index`, and the
first fragment for each call carries `id`, `type`, and
`function.name`, per the OpenAI chunk shape. Terminal chunk:
`finish_reason: "tool_calls"`.

Per-turn usage: the CLI's `result` line (today's usage source) only
appears when the whole run ends, which for a session is *after the
last* turn. Mid-session responses take usage from the
`message_delta` stream event's `usage` fields instead; carry the same
cost/usage enhancements (`cost_usd`) where the data exists, and omit
rather than fabricate where it doesn't.

### 5. The pause

Each MCP `tools/call` request arriving on a session:

1. Matches it to an announced-but-unresolved tool call from §4, by
   `(name, canonicalized arguments)` in announcement order (FIFO
   within identical pairs — two identical calls resolve in arrival
   order; acceptable because their results are interchangeable only
   if the client says so by id, and ids were assigned in the same
   announcement order the CLI dispatches in).
2. Records `{tool_call_id, resolver}` on the session — a channel in
   Go — and does **not** respond. The CLI's JSON-RPC request sits
   open; §2's `MCP_TOOL_TIMEOUT` guarantees the CLI won't give up
   before the gateway does.
3. The CLI process idles, blocked on its MCP read, ~0 CPU.

A `tools/call` naming a tool that was never announced on that session
(or exceeding the announced count) is a protocol violation: respond
with a JSON-RPC error and log it — do not silently park.

### 6. Resuming: correlation by tool_call_id

`tool_call_id`s are **gateway-minted, globally unique, and echoed back
verbatim** by every compliant OpenAI client in its `{role: "tool",
tool_call_id, content}` messages. They are therefore the session key —
no prefix hashing, no content matching:

- Maintain one global map `tool_call_id → sessionId` (entries created
  at finalization, deleted on resolution or reap).
- On each incoming request, collect the trailing `tool` messages. If
  any `tool_call_id` hits the map, the request is a continuation of
  that session: resolve each pending call's resolver with the matching
  message's `content` (string, or text parts joined — same rules as
  `textOf`; MCP result shape `[{type:"text", text}]`). The parked
  `tools/call` responses unblock, the CLI resumes, and the handler
  goes back to relaying stream output — same code path as any turn.
- Requests whose trailing tool messages match *no* live session fall
  through to §3's cold-history flattening.
- Mixed/partial results (client sends results for 2 of 3 pending
  calls): resolve what arrived; if the turn cannot complete because a
  call is still unresolved, hold the HTTP response until the remaining
  result arrives on a later request or `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS`
  fires. In practice OpenAI clients send all results in one request;
  this is a robustness path, not a hot path.

Why not the prefix hash from revision 1: two parallel runs of the same
agent commonly share an identical prefix (same system prompt, same
first user message) — a prefix hash cross-wires their sessions.
Byte-level hashing is also brittle against clients that re-serialize
JSON (key order, null elision). `tool_call_id` has neither problem and
is contractually round-tripped.

### 7. Lifecycle: sessions own processes, requests don't

Today the CLI child is killed when the HTTP request ends or aborts
(`req.on('close')` → `AbortController` → SIGKILL). For sessions this
inverts: **the session table owns the process**; an HTTP request is
just a window onto it. The first request closing after its
`tool_calls` response is the *normal* case and must not kill anything.
Client abort mid-generation still cancels — but "cancel" for a session
means: kill the CLI, reap the session, and delete its
`tool_call_id` map entries, so a later continuation cleanly falls
through to cold history (§3).

Timeouts (both enforced by one background sweeper):

- `CLAUDE_GATEWAY_TOOL_TIMEOUT_MS` (default 120000) — max time a
  parked call waits for the client's result. On expiry: SIGKILL the
  CLI, reap the session, respond to nothing (the HTTP response already
  went out); a later continuation gets cold-history treatment.
- `CLAUDE_GATEWAY_SESSION_IDLE_MS` (default 600000) — reaps sessions
  with no pending calls and no activity (client abandoned the
  conversation after a completed turn).
- Supersession — reaped inline, not by the sweeper. A request whose
  history names a live session's pending `tool_call_id`s but does not
  carry their results has abandoned that loop (an interactive frontend
  interrupting mid-call, replying with a new user turn instead). It takes
  the cold path (§3), and the session it walked away from is reaped as
  the superseding request is served, rather than squatting on a
  `MAX_SESSIONS` slot until the tool timeout. Correlation is by minted
  `tool_call_id` (§6), so unrelated concurrent sessions — including
  byte-identical parallel conversations — are never touched.
- The existing `CLAUDE_GATEWAY_TIMEOUT_MS` becomes **per-turn** for
  sessions (spawn-to-turn-end, resume-to-turn-end), not per-process —
  a healthy multi-turn session legitimately outlives 300s of wall
  clock.

### 8. Resource accounting

The existing semaphore counts *actively generating* processes. A
parked session isn't generating; counting it against
`CLAUDE_GATEWAY_CONCURRENCY` would let a few slow tool round-trips
(human-in-the-loop tools, slow APIs) starve all other traffic.

- Release the concurrency slot when a session parks; re-acquire on
  resume (FIFO with everyone else).
- New `CLAUDE_GATEWAY_MAX_SESSIONS` (default 16) bounds total live
  CLI processes (active + parked), since each parked session still
  holds a process, memory, and a session-table entry. At the cap,
  new tool-carrying requests get `429` with the OpenAI error envelope.

## What stays true

- **No execution on the gateway host.** The relay's `tools/call`
  handler never runs anything — it relays the proposal out and the
  result back in. With `--tools ""` + `--strict-mcp-config`, the model
  can reach exactly the tools the client itself declared, and nothing
  else. Same security posture as today, extended to a case that
  previously wasn't possible.
- **Requests without `tools` are untouched.** Zero behavior change for
  every existing caller.

## Known limitations / accepted tradeoffs

- **In-memory only.** Paused sessions die with the gateway process.
  Deliberate: persistence would contradict the zero-dependency stance,
  and §3's cold-history flattening makes the failure degraded-but-
  recoverable rather than fatal.
- **`tool_choice` enforcement is partial.** `"none"` is honored (skip
  registration). `"auto"` is the natural behavior. `"required"` and
  forced-specific-tool have no CLI flag; steer via an appended system
  prompt line and document as best-effort — do not silently ignore.
- **Cold-history fidelity.** Flattened tool exchanges (§3) are prose
  markers, not native tool_use blocks; model behavior after a gateway
  restart may differ subtly from a live session. Documented, not
  hidden.
- **Single process.** The session table is process-local; running N
  gateway replicas behind a load balancer breaks resume unless the LB
  is session-sticky. Out of scope for a localhost-first tool.

## Open questions to verify before implementation

1. Which MCP HTTP transport version the installed `claude` build
   speaks (Streamable HTTP vs. legacy HTTP+SSE).
2. Whether the CLI dispatches multiple `tools/call`s for one turn
   concurrently or strictly sequentially. §4's design is correct
   either way, but §5's FIFO matching should be tested under both.
3. Confirm `MCP_TOOL_TIMEOUT` / `MCP_TIMEOUT` env var names and
   semantics against the installed build (§2.3).
4. Confirm the stream-json `assistant` message for a tool-use turn
   carries complete `tool_use` blocks (id, name, full input) — the
   finalization path (§4) depends on it.

## Implementation phases

1. **Plumbing.** Session table + sweeper, MCP HTTP endpoint
   (`initialize`, `tools/list`, `tools/call` echoing a stub result),
   spawn-with-mcp-config wiring, `MCP_TOOL_TIMEOUT` env. No
   client-visible behavior change.
2. **Pause/resume.** Stream-driven finalization (§4, non-streaming
   first), parked `tools/call`s (§5), tool_call_id correlation (§6),
   request/session lifetime split (§7).
3. **Completeness.** Streaming tool-call deltas, per-turn usage,
   cold-history flattening (§3), timeouts, `MAX_SESSIONS`, slot
   release/re-acquire (§8).
4. **Proof.** Docs (README + spec.md §11) and a parity harness
   extending `TestAPIParity`: a stub `claude` binary that speaks just
   enough MCP to run a scripted two-turn conversation (announce
   tool_use → park → client sends result → resume → final answer),
   plus a sequential-dispatch stub proving the §4 deadlock cannot
   recur, and a two-identical-sessions test proving correlation
   doesn't cross-wire.

## Revision history

- **r2 (design review).** Fixed a finalization deadlock: r1 waited for
  all N MCP `tools/call`s before responding, which deadlocks under
  sequential CLI dispatch — finalization is now driven by the
  stream-json announcement alone (§4). Replaced prefix-hash session
  correlation, which cross-wires identical-prefix parallel runs and is
  brittle to JSON re-serialization, with gateway-minted `tool_call_id`
  correlation (§6). Detached session lifetime from HTTP request
  lifetime — r1 left today's on-request-close SIGKILL in place, which
  would have killed every session at first-response time (§7). Added:
  cold-history flattening (§3), per-turn usage sourcing (§4), child
  `MCP_TOOL_TIMEOUT` env (§2), per-turn semantics for the existing
  timeout (§7), `tool_choice: "none"` handling, session-id entropy
  requirements (§1), a `--resume`-based alternative considered and
  rejected, and concrete adversarial tests in phase 4.
