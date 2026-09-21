# M1 implementation plan

Status: implementation and exit criteria completed on 2026-09-03; awaiting the
final completion-marker CI run and `milestone-m1` tag.

## Objective

Deliver one durable local-native coding loop. `erka run <prompt>` must operate
directly in the current repository, persist its task and transcript in SQLite,
execute only workspace-scoped tools, survive a process restart, and support a
native DeepSeek connection without making the provider part of runtime state.

This document stays local under the ignored `docs/` tree. Public progress is
represented by code, tests, the standalone README, atomic commits, and the
`milestone-m1` tag.

## Delivery order

### P0.1 — Durable domain and reference store

- Add project, task, and run records needed by the first vertical slice.
- Extend the store boundary with creation and ordered transcript/event reads.
- Implement an in-memory store that enforces optimistic sequences, state
  transitions, transcript relationships, and atomic completion.
- Add deterministic ID and clock injection for runtime/application tests.

**Checkpoint:** project/task/run creation and replay pass without SQLite.

### P0.2 — SQLite and migrations

- Add `modernc.org/sqlite` and `golang-migrate/migrate/v4`.
- Embed forward-only schema migrations for projects, tasks, runs, runtime items,
  events, and schema metadata.
- Store provider-neutral payloads and use transactions for items, events, and
  task status changes.
- Enforce sequence compare-and-swap behavior and stable unique IDs.
- Reopen the database in tests and prove committed transcript/event recovery.

**Checkpoint:** clean migration, reopen, rollback-on-error, and replay tests pass.

### P0.3 — Deterministic native loop

- Assemble fragmented streamed text and tool-call arguments.
- Reject malformed, conflicting, incomplete, or unknown tool calls.
- Persist each call before execution and each result before the next model
  request.
- Bound iterations, model output, cumulative tool output, and stage duration.
- Handle cancellation and close streams on every exit path.
- Recover incomplete read-only calls by explicit replay; pause an incomplete
  mutating call whose outcome is unknown.
- Provide a scripted provider and fake tools for deterministic tests.

**Checkpoint:** the scripted loop passes stream, cancellation, iteration-limit,
ordering, replay, and unknown-mutation recovery tests.

### P0.4 — Workspace sandbox and tools

- Implement a local sandbox rooted at a canonical workspace directory.
- Reject absolute paths, traversal, symlink escapes, special files, binary
  content where unsupported, and oversized reads.
- Run commands with a minimal allowlisted environment, bounded stdout/stderr,
  timeouts, context cancellation, and process-group cleanup where supported.
- Implement `read_file`, `list_files`, `search_text`, `apply_patch`,
  `run_command`, `git_status`, and `git_diff`.
- Do not provide commit, push, pull-request, deployment, or provider credentials
  to tool subprocesses.

**Checkpoint:** adversarial path and process tests pass on supported platforms.

### P1.1 — Local application and CLI

- Resolve the current working repository and create/open the state database.
- Create a local project, task, and coding run for `erka run`.
- Render committed events synchronously without starting a daemon or HTTP
  server.
- Add configuration for the selected native connection and loop/tool limits,
  preserving flag/environment/config/default precedence.
- Make restart behavior explicit and testable from the application layer.

**Checkpoint:** a scripted application invocation edits the fixture, tests it,
shows its diff, and records a completed task.

### P1.2 — DeepSeek native adapter

- Use the documented DeepSeek chat-completions streaming API directly.
- Translate tools and canonical items at the adapter boundary.
- Parse fragmented SSE text, tool calls, usage, completion, and provider errors.
- Read the credential only in trusted application/provider code and construct a
  provider-specific HTTP client with deadlines.
- Keep network tests mocked; place the real API scenario behind an explicit
  opt-in environment flag.

**Checkpoint:** adapter conformance tests and the optional fixture integration
path are present; the standard suite requires no secret or network.

## Planned public commit sequence

1. `feat(store): add durable task domain and memory store`
2. `feat(store): add embedded SQLite persistence`
3. `feat(runtime): add deterministic native tool loop`
4. `feat(sandbox): enforce local workspace boundaries`
5. `feat(tools): add scoped coding tools`
6. `feat(app): run local tasks through the durable runtime`
7. `feat(provider): add native DeepSeek streaming`
8. `test: prove the M1 fixture workflow and recovery`
9. `docs: document the local native vertical slice`
10. `chore: mark M1 completion`

Small fixes discovered by CI remain separate commits. Every commit is checked
and pushed immediately. Published history is never rewritten.

## Exit evidence

- `AGENTS.md` and `docs/` remain ignored and have no tracked descendants.
- `go test -race ./...`, `go vet ./...`, and `go build ./cmd/erka` pass.
- GitHub Actions is green for Go 1.26/1.27 and macOS/Linux/Windows builds.
- The deterministic fixture asserts the final diff, transcript ordering, event
  ordering, and completed state.
- SQLite restart tests cover an incomplete read-only call and an unknown
  mutating call outcome.
- No provider secret is persisted, emitted, placed in model context, or exposed
  to tool subprocesses.
- Local `erka run` starts no server and uses no network except its selected model
  provider.
- The verified boundary is tagged `milestone-m1` only after CI succeeds.
