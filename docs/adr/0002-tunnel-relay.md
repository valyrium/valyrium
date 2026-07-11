# ADR 0002: Self-hosted tunnel/relay for remote reachability

## Status

Proposed. Not yet implemented.

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

## Open questions to verify before implementation

1. Confirm the mux protocol doesn't need per-stream flow control for v1
   (i.e., a single slow client can't be used to exhaust relay memory) —
   likely fine given personal single-tenant use, but bound stream buffer
   sizes regardless.
2. Confirm exact backoff/reconnect parameters for `valyrium tunnel`
   against real-world home ISP disconnect/reassign behavior.
3. Decide whether `valyrium relay` should refuse to start if
   `VALYRIUM_TUNNEL_TOKEN` is unset (recommended: yes, hard-fail).
