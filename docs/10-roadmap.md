# Delivery roadmap

Every milestone ends with a demonstrable user outcome. Do not start broad
integrations until the previous milestone's exit criteria pass.

## M0 — Foundation and contracts

- Initialize the Go module and `erka` command.
- Define canonical model input/output and runtime event types.
- Define store, model provider, execution backend, tool, sandbox, and event sink
  boundaries.
- Add configuration loading and platform-standard application directories.
- Set up tests, linting, release builds, and architecture decision records.

**Exit:** `erka version`, `erka help`, configuration tests, and cross-platform
build jobs succeed.

## M1 — Local native vertical slice

- SQLite store and embedded migrations.
- Local project and task creation.
- DeepSeek native streaming adapter.
- `read_file`, `list_files`, `search_text`, `apply_patch`, `run_command`,
  `git_status`, and `git_diff`.
- Workspace path enforcement, output caps, timeouts, and cancellation.
- One-model local loop with persisted tool transcript.

**Exit:** `erka run` completes a fixture-repository change, tests it, shows the
diff, and resumes correctly after restart.

## M2 — Pipeline and multi-model routing

- Planning, coding, validation, repair, and review stages.
- Fixed routes and deterministic fallback rules.
- Stage-specific context compiler and checkpoints.
- Usage, cost metadata, and stage budgets.
- Native OpenAI API adapter.

**Exit:** one provider plans and another implements the same task without losing
decisions or replaying the entire transcript.

## M3 — Subscription bridges

- Codex device login and subscription bridge using an official Codex surface.
- Bounded plan/review execution and canonical event translation.
- OpenCode Go bridge with the same explicit bridge labeling.
- Bridge capability and availability reporting.

**Exit:** a ChatGPT-subscription-backed planning stage hands a durable plan to a
DeepSeek native coding stage.

## M4 — Optional server mode

- `erka serve` daemon.
- Versioned API, authentication, durable worker, SSE resume, approvals, and
  server-side worktrees.
- Remote CLI profiles and commands.
- Generic webhook notifications.

**Exit:** a task submitted from one device continues after disconnection and can
be reviewed and approved from another device.

## M5 — Web client

- Projects, task creation, live activity, conversation, diff, validation,
  approvals, routing, and provider settings.
- Responsive/PWA behavior and browser notifications.
- Reuse selected visual ideas from AI-LXC only when they fit the new API.

**Exit:** the primary remote workflow needs no terminal access to the server.

## M6 — Extensibility and public release

- Generic OpenAI-compatible provider profiles.
- GLM conformance profile or adapter.
- Provider contribution test kit.
- Official MCP Go SDK integration.
- On-demand skills.
- Installers, signed/checksummed binaries, minimal server image, SBOM, security
  policy, contribution guide, and migration documentation.

**Exit:** a new user can install locally in minutes, while a contributor can add
and verify a provider without changing runtime core behavior.

## Deferred

- Multi-user hosted SaaS.
- Distributed worker clusters.
- Native mobile applications.
- Local-worker-to-remote-control-plane bridging.
- Automatic deployment without approval.
- Provider or skills marketplace.

