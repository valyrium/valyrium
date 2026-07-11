# ADR 0002: Self-hosted tunnel/relay for remote reachability

## Status

Accepted. Implemented in `internal/tunnel` (`mux.go`, `relay.go`, `tunnel.go`),
wired to the `valyrium relay` and `valyrium tunnel` subcommands. The open
questions below are resolved at the end of this document.

## Context

`valyrium` binds to `127.0.0.1` by default (docs/spec.md §3) and is meant
to be reached by local tooling only. There is now a need for cloud-hosted
services to reach a `valyrium` instance running on a private network
(home LAN, no public IP, no inbound port forwarding), so that those
services can use the local Claude Code CLI login instead of a separate
API key.

This requires some form of reverse tunnel: the home instance dials *out*
to a publicly reachable relay, and the relay forwards inbound public
traffic back down that connection. The public side also needs real TLS
(Let's Encrypt), since it is calling from services that expect a valid
certificate, not a self-signed one.

Following the dependency policy update in docs/spec.md, `golang.org/x/...`
packages are allowed alongside the standard library; no other third-party
modules are.

## Alternatives considered

- **Use an existing tunnel tool (Tailscale Funnel, `cloudflared`,
  `ngrok`, `frp`) as a separate process alongside `valyrium`.** Simplest,
  most battle-tested, gets TLS and DDoS protection for free. Rejected
  as the *only* answer per explicit direction to build this in-house;
  still the fallback if the custom relay proves not worth maintaining.
- **Terminate TLS at the relay by proxying raw bytes at the HTTP layer
  (relay parses HTTP, extracts routing info, re-issues a new request to
  the tunnel client).** Adds an HTTP parser/rewriter on the relay for no
  benefit here — single-tenant, single-hostname — and risks subtly
  breaking chunked transfer / SSE streaming (`valyrium` streams
  responses). Rejected in favor of a byte-level (L4) pipe.
- **Wildcard subdomain per tunnel with DNS-01 challenges, for eventual
  multi-tenant support.** `autocert` does not support DNS-01 out of the
  box (would need a custom `acme.Client` challenge solver against a DNS
  provider API). Deferred — v1 is single-tenant, one fixed hostname,
  HTTP-01 is sufficient.

## Decision

Build two new components, both new subcommands of the existing
`valyrium` binary (`valyrium relay` and `valyrium tunnel`), sharing one
hand-rolled stream-multiplexing protocol over a single outbound TCP/TLS
connection. The relay terminates public TLS via
`golang.org/x/crypto/acme/autocert`; everything past that is an
undifferentiated byte pipe down to `127.0.0.1:8787`, so `valyrium`'s
existing `CLAUDE_GATEWAY_API_KEY` auth is the only thing that gates
access to the API itself — the relay adds no app-layer trust of its own
beyond authenticating *which tunnel client* it's piping to.

```
cloud client ──HTTPS(LE cert)──▶ valyrium relay ──mux/TLS──▶ valyrium tunnel ──127.0.0.1:8787──▶ valyrium
                                  (public VPS)                (home network)
```

### 1. Transport: hand-rolled stream multiplexer

One control connection (TCP+TLS, client-initiated from home → relay)
carries many logical streams, framed as:

```
[4-byte length][1-byte type][4-byte stream-id][payload]
```

Types: `OPEN`, `DATA`, `CLOSE`, `PING`/`PONG`. Built from
`encoding/binary`, `bufio`, `net`, `sync` — no muxer library. Either side
may open a stream; in practice only the relay does (one per inbound
public connection). A single writer goroutine per underlying connection
serializes frames; each open stream gets a buffered channel for reads so
one slow stream can't head-of-line-block the others.

Chosen over running real HTTP/2 for the tunnel link: HTTP/2 in
`net/http`/`golang.org/x/net/http2` assumes a client-opens-streams model
and doesn't map cleanly onto "relay opens streams toward the dialer."
A ~150-line custom frame protocol is simpler than bending HTTP/2 to a
shape it wasn't built for, and this is the same approach existing tools
in this space (frp, chisel, inlets) use.

### 2. `valyrium relay` (new binary target, public VPS)

- Listens on `:443`. TLS via `autocert.Manager` (`HTTPHandler` on `:80`
  for HTTP-01, `HostPolicy` pinned to one configured hostname,
  `DirCache` for on-disk cert persistence across restarts).
- Demuxes connections by negotiated ALPN protocol: `"vtun/1"` is a
  tunnel-client control connection; anything else (`h2`, `http/1.1`) is
  a public API caller.
- Tunnel-client connections must present a bearer token (constant-time
  compare, same pattern as `CLAUDE_GATEWAY_API_KEY`) before the relay
  will treat them as the active mux endpoint. Only one tunnel client may
  be registered at a time in v1 (single-tenant); a second connection
  attempt with a valid token replaces the first (last-writer-wins, so a
  reconnect after a network blip doesn't get locked out).
- Public connections: for each accepted TLS connection, open a new mux
  `OPEN` stream to the registered tunnel client, then pipe bytes
  bidirectionally (`io.Copy` both directions) between the public
  `net.Conn` and the mux stream until either side closes.
- If no tunnel client is currently registered, public connections get
  an immediate `503` (plain HTTP response written directly, since there
  is nothing to proxy to).

### 3. `valyrium tunnel` (new subcommand, runs at home next to `valyrium`)

- Dials the relay's `:443` with ALPN `"vtun/1"`, sends the bearer token,
  blocks as the mux's connection-accepting side.
- For each `OPEN`ed stream, dials `127.0.0.1:8787` (or configured local
  address), pipes bytes both ways, `CLOSE`s the stream when either side
  is done.
- Reconnects with exponential backoff (capped) on control-connection
  loss; local `valyrium` process is untouched by tunnel connectivity
  state.
- `PING`/`PONG` keepalive on an interval shorter than any intermediate
  NAT/firewall idle-connection timeout.

### 4. Configuration (env vars, following existing `CLAUDE_GATEWAY_*` convention)

| Var | Where | Meaning |
|---|---|---|
| `VALYRIUM_TUNNEL_DOMAIN` | relay | Public hostname the cert and routing are pinned to |
| `VALYRIUM_TUNNEL_TOKEN` | both | Shared bearer secret authenticating the tunnel client to the relay |
| `VALYRIUM_TUNNEL_CERT_CACHE_DIR` | relay | `autocert.DirCache` path |
| `VALYRIUM_TUNNEL_RELAY_ADDR` | tunnel | `host:443` of the relay |
| `VALYRIUM_TUNNEL_LOCAL_ADDR` | tunnel | Default `127.0.0.1:8787` |

Two more were added during implementation, both to keep the listen addresses
out of the code: `VALYRIUM_TUNNEL_LISTEN_ADDR` (relay, default `:443`) and
`VALYRIUM_TUNNEL_HTTP_ADDR` (relay, default `:80`). `VALYRIUM_TUNNEL_DOMAIN`
is also read by the tunnel client, where it sets the name the relay's
certificate is verified against when that differs from the address dialed.

### 5. Security posture

- The relay is a dumb pipe: it never terminates or inspects the
  application protocol past the initial ALPN demux. `valyrium`'s own
  `CLAUDE_GATEWAY_API_KEY` check is what actually gates API access once
  traffic reaches the loopback port — **tunneling without that key set
  is equivalent to exposing an open gateway to the internet** and must
  be called out loudly (README warning + relay startup log line if a
  registered tunnel client is later observed proxying to a port that
  responds `auth: open`... deferred; at minimum, document it).
- Bearer-token check on the control channel prevents a third party from
  registering as the tunnel endpoint and hijacking traffic meant for the
  real home instance.
- Single-tenant in v1: no per-tunnel routing, no wildcard cert, no
  multi-home-instance support. Documented as a v1 boundary, not an
  oversight.

## What stays true

- No change to `valyrium` itself or its existing auth model — the
  tunnel components are additive, separate subcommands.
- `golang.org/x/crypto/acme/autocert` is the only new dependency,
  per the updated policy in docs/spec.md.

## Known limitations / accepted tradeoffs

- Single tenant, single hostname, HTTP-01 only (no wildcard/DNS-01).
- Relay is a single point of failure and a single process (no HA);
  acceptable for a personal-use tunnel.
- Relay requires a public VPS + DNS record pointed at it — infrastructure
  provisioning is out of scope for this ADR and assumed as a prerequisite.
- No built-in rate limiting on the public side beyond `valyrium`'s
  existing concurrency semaphore once traffic reaches it.

## Resolved open questions

1. **Per-stream flow control: not in v1, but memory is bounded.** A frame's
   payload is capped at 64 KiB and `ReadFrame` rejects a larger *declared*
   length before allocating, so a four-byte header cannot be turned into an
   arbitrary allocation. Each stream buffers 16 frames (~1 MiB); past that,
   delivery blocks. The honest consequence is that a stalled consumer
   backpressures the *whole* control connection rather than just its own
   stream — head-of-line blocking, not memory growth, and not data loss.
   For a single-tenant personal tunnel where every consumer is an `io.Copy`
   into a TCP socket that always eventually drains, that is the right trade.
   Revisit with per-stream windows if this ever fronts many concurrent slow
   clients.
2. **Backoff: 1s doubling to 30s**, reset once a connection has survived
   longer than the maximum backoff (so a relay that accepts and instantly
   drops cannot be hot-looped). No jitter: a single-tenant tunnel has no
   thundering herd to avoid. `PING` every 30s against a 90s idle deadline on
   both ends, which is inside the idle timeout of typical consumer NAT.
   Not yet validated against a real ISP disconnect/reassign — the parameters
   are a starting point, and `TestTunnelReconnectsAfterDrop` only proves the
   client recovers from a relay that vanishes.
3. **Yes, hard-fail.** `NewRelay` refuses to build a relay with no token, so
   `valyrium relay` exits non-zero rather than starting up ready to hand
   traffic to whoever connects first. Serving public TLS additionally
   requires `VALYRIUM_TUNNEL_DOMAIN` and `VALYRIUM_TUNNEL_CERT_CACHE_DIR`;
   both are checked before anything binds.

## Decided during implementation

- **The relay does not offer `h2`.** ALPN is `vtun/1`, `http/1.1`, and
  `acme-tls/1` (the last so TLS-ALPN-01 challenges keep working). The byte
  pipe itself is protocol-agnostic and would carry h2 happily, but the one
  response the relay generates *itself* — the 503 when no tunnel client is
  registered — is written by hand as HTTP/1.1, and an h2 client could not
  read it.
- **The 503 path reads the caller's request before answering it.** Writing
  the response the instant the handshake completes looks harmless and is
  not: to any HTTP client that pools connections, a response that arrives
  before the request was written is "unsolicited", and it is discarded in
  favour of a transport error. So for that one connection the relay behaves
  like an HTTP server — read the request, drain up to 1 MiB of body so an
  uploading caller is not reset mid-write, then answer.
- **Frame types include `AUTH`/`AUTH_OK`**, used only for the handshake that
  precedes the mux. Both ends read them unbuffered, straight off the
  connection, so no byte of the mux stream that follows is swallowed into a
  buffer the multiplexer will never look in.
- **Stream ids are parity-split** (relay odd, tunnel even) so both ends may
  open streams without ever colliding, even though only the relay does.
