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

### Changed
- Interactive sessions offer `run_command` with per-call consent instead of
  hiding it until full access is enabled.
- Ctrl+C clears the input and quits on a second press instead of a dialog.
- Design documents and ADRs are tracked in Git.
