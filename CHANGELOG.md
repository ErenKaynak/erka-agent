# Changelog

All notable changes to erka are recorded here. The project follows
[Semantic Versioning](https://semver.org); versions before 1.0 may change the
configuration and API between minor releases.

## Unreleased

### Added
- Mid-run `ask_user` questions and per-command permission requests, answered
  inline in the TUI or remotely; server tasks park durably and resume after
  the answer.
- Durable notification outbox with ntfy (including action buttons) and signed
  generic webhook targets.
- VPN-only serving with `--trusted-cidr` / `server.trusted_cidrs`, and plain
  HTTP remote profiles for private and Tailscale addresses.
- Long-term memory shared by every model and bridge, with `erka memory`
  commands, `memory_search` / `memory_save` tools, and a `/v1/memories` API.
- Complexity-tiered routing (`routing: tiered`) with heuristic or model triage.
- Managed dev-server previews for server tasks from configured presets.
- Mobile web client at `/app/` for questions, approvals, activity, diffs,
  previews, and memory.
- Redesigned single-column TUI: inline tool steps, live status with elapsed
  time and usage, grouped model picker, filterable conversation picker,
  colored diff viewer, and light/dark themes.
- Auto, plan (read-only), and god (full access) modes cycled with Shift+Tab.
- `erka -s <session>` resumes a conversation by ID prefix; quitting prints the
  command.
- `erka version` reports the Git commit and build time for development builds.
- Accepting a server task commits its worktree to `erka/<task>` and removes
  the worktree; rejecting keeps the changes at `refs/erka/rejected/<task>`.
  Finalization is crash-safe, never runs repository hooks, and closes the task.
- OS sandbox for model commands and previews (`sandbox-exec` on macOS,
  bubblewrap on Linux): writes limited to the workspace, loopback-only network
  by default, credential stores and erka state hidden, and Git hooks and
  config read-only. Configure with `sandbox.mode`, `sandbox.network`, and
  `sandbox.writable_paths`; god mode bypasses it.
- erka's file tools refuse to write repository metadata under `.git`.
- `erka serve` works with no configuration: it listens on loopback and any
  detected Tailscale or WireGuard address, trusts only those networks,
  registers the current repository, and prints a QR code for one-time device
  pairing (`/v1/pair`, single use, ten-minute expiry, rate limited).
  `erka server pair` prints a new code.

### Changed (serve)
- The server offers `run_command` with per-call approval by default; set
  `permissions.approve_commands: false` to hide it.
- `--listen` now means "exactly this address"; the default is automatic.

### Changed
- Interactive sessions offer `run_command` with per-call consent instead of
  hiding it until full access is enabled.
- Ctrl+C clears the input and quits on a second press instead of a dialog.
- Design documents and ADRs are tracked in Git.
