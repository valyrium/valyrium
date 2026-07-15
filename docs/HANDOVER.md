# Handover notes

Written at the end of the session that renamed this project to valyrium and
cut its first public release. The next session may be on a different
machine with none of this machine's local state (no local Homebrew install
of valyrium, no local SSH/gh auth for the `valyrium` org, etc.) — everything
that matters should be captured here or already committed in the repo.

## Where things live

- Main repo: `github.com/valyrium/valyrium` (renamed from `dndungu/llm-gateway`;
  the module path, binary name, and CLI are all `valyrium` now).
- Homebrew tap: `github.com/valyrium/homebrew-tap`, formula at
  `Formula/valyrium.rb`.
- Current release: `v1.2.0`, published via GitHub Releases with cross-compiled
  archives for linux/darwin × amd64/arm64 and windows × amd64/arm64. The
  release pipeline is now fully automated end-to-end (see "What's done").
- Release automation: `release-please` (version PRs + tags) +
  `GoReleaser` (cross-compile, GitHub release, Homebrew formula), wired in
  `.github/workflows/release-please.yml`, `.goreleaser.yml`,
  `release-please-config.json`, `.release-please-manifest.json`.

## What's done

- Full Go port of the original TypeScript llm-gateway reference (now deleted
  — see `docs/spec.md` for the current, Go-only spec).
- MCP-relay tool calling (ADR 0001: `docs/adr/0001-mcp-relay-tool-calling.md`),
  letting OpenAI-style tool-calling clients (tested against the real
  `opencode` CLI, not just the test suite's stub) use `claude -p` as a backend
  while executing tools client-side.
- Two real bugs found by that opencode dogfood run and fixed (both have
  regression tests): the gateway was forwarding Claude Code's internal
  `mcp__relay__<tool>` qualified tool name straight to OpenAI clients instead
  of stripping it back to the bare name they declared; and streaming
  responses (plain and tool-calling) never wrote token usage back to the
  structured stderr request log.
- Renamed the whole project to valyrium and moved it to the `valyrium` GitHub
  org (see "Naming decisions" below for why).
- Release pipeline bootstrapped, v1.0.0 cut and verified live: `brew install
  valyrium/tap/valyrium`, ran the binary, hit `/healthz`, and drove one real
  chat completion through the actual `claude` CLI to the real Anthropic API.
- Homebrew formula has a `service do` block (launchd), verified with
  `brew services start` — starts at login, restarts on crash, and its `PATH`
  is extended (portably, via `Dir.home` — not hardcoded to any one machine's
  username) with `~/.local/bin`, `~/.claude/local`, `/opt/homebrew/bin`, and
  `/usr/local/bin` so it can find a user-installed `claude` CLI.
- **Release pipeline fully automated (as of v1.2.0).** The
  `HOMEBREW_TAP_GITHUB_TOKEN` secret is set on `valyrium/valyrium`, so merging a
  release-please PR now cuts the tag, publishes the GitHub release, AND pushes
  the updated Homebrew formula to `valyrium/homebrew-tap` with no manual step.
  First proven on the v1.2.0 release (the whole Release workflow went green;
  the tap got its "Brew formula update for valyrium version v1.2.0" commit
  automatically).
- **CI test gate on PRs.** `.github/workflows/ci.yml` runs `go vet`, a `gofmt`
  check, and `go test -race ./...` on every pull request and push to `main`.
- **`claude` CLI discovery hardened (v1.2.0).** The gateway resolves the
  `claude` binary at startup — prefers `PATH`, then probes `~/.local/bin`,
  `~/.claude/local`, `/opt/homebrew/bin`, `/usr/local/bin` — and returns an
  absolute path. If `claude` is nowhere, it refuses to start with an error
  naming every location searched and pointing at `CLAUDE_GATEWAY_BIN`, instead
  of the old failure mode where `/healthz` looked healthy but every chat
  request failed to spawn. See `cmd/valyrium/claudebin.go`.

## What's NOT done / needs attention

The four items that lived here through v1.1.0 — the open release-please PR, the
missing `HOMEBREW_TAP_GITHUB_TOKEN` secret, the fragile `claude` discovery, and
the absent CI gate — are all resolved as of v1.2.0. See "What's done" above.
What remains:

1. **Only `claude` install locations known at build time are probed.** The
   startup discovery in `cmd/valyrium/claudebin.go` and the formula's service
   `PATH` cover `~/.local/bin`, `~/.claude/local`, `/opt/homebrew/bin`, and
   `/usr/local/bin`. A user whose `claude` lives elsewhere still needs to set
   `CLAUDE_GATEWAY_BIN` — but now they get a clear startup error telling them
   so, rather than silent per-request spawn failures. If a new common install
   path emerges, add it to `candidateBinDirs()` and to the `service` block's
   `PATH` in `.goreleaser.yml` (keep the two lists in sync — there's a comment
   in `claudebin.go` saying as much).

2. **The token scope should be spot-checked periodically.**
   `HOMEBREW_TAP_GITHUB_TOKEN` is a fine-grained PAT and will expire on its
   configured date; when it does, releases will start failing at the tap-push
   step again (401), exactly as they did before it was set. If that happens,
   regenerate the PAT (`contents:write` on `valyrium/homebrew-tap` only) and
   update the secret. As a stopgap for a single release, the old manual path
   still works: clone `valyrium/homebrew-tap`, bump `version`/`url`/`sha256` in
   `Formula/valyrium.rb` to match the new assets (checksums via `gh release
   download <tag> --repo valyrium/valyrium --pattern "*checksums.txt"`),
   commit, push.

3. **No end-to-end / integration coverage of the running service** beyond the
   unit suite and a manual smoke test — nothing exercises an actual
   `brew install`ed launchd service driving a real request in CI. Fine for a
   solo-maintainer repo, but worth considering before accepting outside
   contributions.

## Naming decisions (context for "why is it called this")

- Two GitHub orgs were parked: `valyrium` (correct spelling, created 2020,
  1 member, no display name, 2FA off — looked like an unused placeholder) and
  `valirium` (misspelled handle, created 2018, 2 members, 2FA required, and
  its GitHub *display name* was already set to "Valyrium" — looked like the
  actively-configured org). I flagged this discrepancy; the user chose
  `valyrium` (the correctly-spelled handle) to consolidate on going forward.
- The project itself (previously "llm-gateway") was renamed to `valyrium` to
  match — module path `github.com/valyrium/valyrium`, binary `valyrium`.
- `CLAUDE_GATEWAY_*` env var names were deliberately left unchanged during the
  rename — they describe function (this gateways Claude), not brand.
- GoReleaser's `brews:` config key is officially deprecated in favor of
  `homebrew_casks:`, but this repo deliberately kept `brews:` — Homebrew Casks
  are a macOS-only mechanism, and this is a cross-platform (Linux + macOS)
  service binary that should install identically via Linuxbrew. `brews:`
  still works, just prints a deprecation notice on `goreleaser check`.

## Machine-specific state that will NOT carry over

- The machine this was built on has `valyrium` actually running as a
  `brew services`-managed launchd service right now
  (`brew services start valyrium/tap/valyrium`). A fresh machine starts with
  none of that; to reproduce: `brew tap valyrium/tap && brew install
  valyrium/tap/valyrium && brew services start valyrium/tap/valyrium`.
- Local git remote is `origin` → `git@github.com:valyrium/valyrium.git` over
  SSH. A new machine needs its own SSH key (or HTTPS + `gh auth login`) with
  push access to the `valyrium` org before it can push here. **On the current
  machine the SSH key is NOT yet authorized** for the org (`git@github.com`
  returns `Permission denied (publickey)`); the v1.2.0 work was all pushed over
  HTTPS (`git push https://github.com/valyrium/valyrium.git main`) as a
  workaround. Add the key to the GitHub account, or switch `origin` to HTTPS,
  to make plain `git push`/`git pull` work.
- `gh` CLI needs to be authenticated with admin rights on the `valyrium` org
  to create repos, manage secrets, merge PRs, etc.
