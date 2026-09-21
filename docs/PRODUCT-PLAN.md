# erka product plan

Goal: turn erka from a personal tool into an open-source product that other
developers install in minutes, trust with their repositories, and keep using
every day in place of Claude Code, Codex, or OpenCode.

Each phase ends with exit criteria that must be demonstrated, not assumed.
A phase does not start until the previous one's criteria pass in CI.

## Who it is for

Developers who pay for several model providers and subscriptions, want one
tool across all of them, and want to keep working from a phone through their
own server. The promise, in order of importance:

1. It never damages a repository or leaks a secret.
2. It is as pleasant as the tools it replaces.
3. It costs less, because work is routed to the cheapest capable model.
4. It works remotely without a vendor cloud.

## Phase 0 — Protect the work (now)

- Feature branch, logical commits, and a pushed backup of all current work.
- Track design documents and ADRs; keep only personal notes ignored.
- CI runs formatting, vet, race tests, and macOS/Linux/Windows builds on
  every change.
- Version the product: `CHANGELOG.md`, semantic versions, `erka version`
  reporting commit and build date.

**Exit:** a clean clone on another machine builds and passes the full suite;
no work exists only in a working tree.

## Phase 1 — Trust: safe by default

A product that can run commands must be trustworthy before it is featureful.

- **OS sandbox** behind the existing sandbox interface: `sandbox-exec` on
  macOS, bubblewrap or Landlock on Linux; writes limited to the workspace,
  network off unless allowed. God mode is the only unsandboxed mode.
- **Accept → branch workflow:** accepting a server task commits its worktree
  to `erka/<task>`, optionally pushes or opens a PR, then removes the
  worktree. Rejecting removes it.
- **Previews behind approval** when a task changed files the preset executes,
  and inside the sandbox.
- **Memory integrity:** model-saved notes are shown in the transcript,
  expire unless confirmed, and are never injected into global scope.
- **Short-lived, single-use action tokens** for notification buttons.
- **Retention and backup:** pruning for events, notifications, logs, and
  worktrees; `erka backup` / `erka restore` for the database.
- **Threat model** document and a security review of every tool boundary.

**Exit:** a hostile fixture repository (prompt injection in files, malicious
`package.json` scripts, symlink escapes) cannot write outside the workspace,
exfiltrate a credential, or run an unapproved command in any mode but god.

## Phase 2 — Parity: replace the tools people already use

- **Anthropic native provider** and a **Claude Code subscription bridge**.
- **Generic OpenAI-compatible profiles** (OpenRouter, Ollama, GLM, vLLM).
- **Context compaction** for long single-model conversations (`/compact`,
  automatic near the limit).
- **Undo checkpoints:** rewind files and conversation to before any turn.
- **MCP client** via the official Go SDK; **skills** loaded on demand.
- **Git tools:** commit, branch, and PR creation with approval.
- **Web fetch/search** tools and **image input** for capable models.
- **Memory sync** between a laptop and a personal server.

**Exit:** a week of daily work on a real project with no fallback to another
agent CLI.

## Phase 3 — Adoption: install and succeed in five minutes

- One-line installer, Homebrew tap, `go install`, signed and notarized
  binaries, SBOM, and checksums from a release pipeline.
- `erka init`: pick providers, store keys in the OS keychain, choose a
  pipeline template (single model, cheap tiered, premium).
- `erka doctor`: checks credentials, sandbox support, Git, and server reach.
- Server packaging: container image, systemd unit, and a Tailscale/WireGuard
  guide with `erka serve` preconfigured for VPN use.
- Documentation site: quickstart, concepts, configuration reference,
  security model, and recipes.

**Exit:** five people who have never seen erka install it and finish a real
change unaided, timed under ten minutes.

## Phase 4 — Reliability: keep working at scale

- Recorded provider fixtures and golden TUI snapshots in CI.
- Fuzzing for stream parsers, patch application, and config loading.
- Crash and restart chaos tests for the server worker, approvals, and outbox.
- Concurrent server workers with per-project limits.
- Opt-in, local-first diagnostics (`erka bug-report`) with secrets redacted.
- Performance budgets: TUI start under 200 ms, 60 fps streaming, bounded
  memory on long sessions.

**Exit:** thirty days of beta use with no data loss and no unexplained crash.

## Phase 5 — Launch

- License decision (Apache-2.0 vs AGPL-3.0) and trademark check for "erka".
- `SECURITY.md` with a disclosure process, `CONTRIBUTING.md`, code of
  conduct, issue and PR templates, and a provider contribution kit.
- Private beta, feedback loop, then public release with a website and demo.

**Exit:** a public 1.0 release, a documented upgrade path, and a security
contact that responds.

## Open decisions for the owner

- License (blocks any public release).
- Whether the product name "erka" is final.
- Whether diagnostics may ever leave the user's machine (default: never).
