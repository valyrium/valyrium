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
- Current release: `v1.0.0`, published via GitHub Releases with cross-compiled
  archives for linux/darwin × amd64/arm64 and windows × amd64/arm64.
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
  is extended with `~/.local/bin` (portably, via `Dir.home` — not hardcoded to
  any one machine's username) so it can find a user-installed `claude` CLI.

## What's NOT done / needs attention

0. **There's an open release-please PR right now**: #3, "chore(main): release
   1.1.0", proposing the version bump for the `feat: add launchd service
   block to the Homebrew formula` commit. It's sitting unmerged — merge it
   (rebase-and-merge) whenever you want v1.1.0 cut. Merging it will tag
   `v1.1.0` and trigger the same GoReleaser flow described in point 1 below
   (GitHub release will succeed; the Homebrew tap push will fail until that
   secret exists, and needs the same manual follow-up).

1. **`HOMEBREW_TAP_GITHUB_TOKEN` secret is not set** on `valyrium/valyrium`.
   The default `GITHUB_TOKEN` GitHub Actions gives a workflow is scoped to the
   repo it runs in only — it cannot push to the separate `homebrew-tap` repo.
   Every future release's CI run will: successfully publish the GitHub
   release (binaries + checksums), then **fail** at the "homebrew formula"
   step with a 401 trying to push to `valyrium/homebrew-tap`. This is expected
   given the current setup, not a regression — I deliberately did not create
   and store a personal access token as a repo secret without checking with
   the user first (it would need broader scope than is comfortable to embed
   in a public repo's CI without an explicit go-ahead).
   - **To fully automate**: create a fine-grained GitHub PAT scoped to
     `contents:write` on `valyrium/homebrew-tap` only, add it as the
     `HOMEBREW_TAP_GITHUB_TOKEN` secret on `valyrium/valyrium`
     (Settings → Secrets and variables → Actions).
   - **Until then**: after merging a release-please PR (which cuts the tag),
     the CI goreleaser job will fail on the tap step. Manually regenerate the
     formula for the new tag and push it to `homebrew-tap`. The fastest way:
     clone `valyrium/homebrew-tap`, copy `Formula/valyrium.rb`, bump the
     `version`/`url`/`sha256` fields to match the new release's assets and
     `valyrium_<version>_checksums.txt` (download via
     `gh release download <tag> --repo valyrium/valyrium --pattern
     "*checksums.txt"`), commit, push. Keep the `service do` block as-is
     unless deliberately changing it.

2. **`claude` CLI discoverability in the service is a soft assumption.** The
   formula's `service` block hardcodes `~/.local/bin` onto `PATH` (portably,
   per-user via `Dir.home`) because that's where `claude` happened to be
   installed on the machine this was built/tested on. If a user's `claude` CLI
   lives somewhere else, the running service will fail every request with a
   spawn error ("binary not found"), even though `brew install` itself
   succeeds and `/healthz` still responds. Worth hardening later — e.g.
   searching a few common install locations, or documenting that
   `CLAUDE_GATEWAY_BIN` can be overridged via a service env var if this
   becomes a real support burden.

3. **No CI test/lint gate yet** beyond what `go test ./...` already covers
   locally — there's no GitHub Actions workflow running the test suite on
   PRs. The release workflow only runs on push to `main`. Fine for a
   solo-maintainer repo at this stage, but worth adding before accepting
   outside contributions.

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
  push access to the `valyrium` org before it can push here.
- `gh` CLI needs to be authenticated with admin rights on the `valyrium` org
  to create repos, manage secrets, merge PRs, etc.
