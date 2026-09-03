# erka-agent

`erka-agent` is a local-first, self-hostable coding-agent runtime written in Go.
It owns the agent loop, tools, context, sessions, routing, permissions, and event
history while treating model vendors as interchangeable intelligence providers.

The user-facing executable is `erka`. Build it and launch the interactive TUI
from any Git repository:

```bash
go build -o erka ./cmd/erka
./erka
```

On first launch, erka asks for the DeepSeek API key in a masked terminal field
and saves it in the operating-system credential store. If the credential store
is unavailable, the key remains in memory for that process only. Environment
credentials take precedence:

```bash
export ERKA_DEEPSEEK_API_KEY='your-key'
./erka
```

The initial keyboard map is:

- `Enter`: submit a prompt or durable follow-up.
- `Alt+Enter` or `Shift+Enter`: insert a newline.
- `Ctrl+O`: open the conversation picker.
- `Ctrl+N`: start a new conversation.
- `Ctrl+D`: inspect the working-tree diff.
- `Ctrl+P`: confirm workspace/full execution for this process.
- `Esc`: close an overlay or confirm cancellation of an active turn.
- `Ctrl+C`: confirm exit; queued prompts remain durable.
- `?`: show keyboard help.

For scripts and one-shot use, `erka run` remains available:

```bash
export ERKA_DEEPSEEK_API_KEY='your-key'

./erka run --full "Fix the failing checkout tests"
```

Run the first task in a disposable repository and review its working-tree diff.
Without `--full`, `erka` exposes the scoped read, search, patch, and Git
inspection tools but does not expose arbitrary command execution.

Use `--cwd <path>` to select another repository and `--task <id>` to open a
specific conversation in the TUI. Each scripted run prints its durable task ID
before contacting the model. If the process
is interrupted, restart it from the same repository with:

```bash
./erka run --full --resume <task-id>
```

An unfinished read-only tool can be repeated safely. An unfinished mutating tool
is left in a waiting state for inspection rather than being silently replayed.

## Configuration

Configuration is optional and strictly parsed as versioned YAML. Command flags
override environment variables, which override the configuration file and then
the built-in defaults. A minimal configuration is:

```yaml
version: 1
provider:
  type: deepseek
  model: deepseek-v4-flash
runtime:
  max_iterations: 24
  max_output_tokens: 8192
  stage_timeout: 30m
permissions:
  allow_full_execution: false
```

Use `--config`, `ERKA_CONFIG_PATH`, and `ERKA_STATE_DIR` to override the
platform-standard locations. The default native endpoint is
`https://api.deepseek.com`; `--provider-base-url` exists for compatible test
endpoints. Secrets do not belong in YAML—`ERKA_DEEPSEEK_API_KEY` is read by the
trusted provider adapter and excluded from tool subprocess environments.

## Product promise

- Run without a server on macOS, Linux, or Windows.
- Run continuously on a self-hosted server and control it from another device.
- Use different models for planning, implementation, repair, and review.
- Keep coding tools and durable task history under the user's control.
- Support native model APIs and clearly labeled subscription bridges.
- Ship as open source without a mandatory hosted service.

## Runtime model

Native providers use the `erka` tool loop end to end. Subscription integrations
are explicit compatibility bridges and are never represented as native model
APIs. Local commands use the same application and runtime core as optional server
mode, but do not require a daemon or hidden HTTP server.

The initial native provider target is DeepSeek. Planned optional integrations
include a native OpenAI API connection and clearly labeled Codex and OpenCode
subscription bridges.

## Roadmap

Development proceeds through demonstrable milestones:

1. Go foundation, command contracts, configuration, tests, and release builds.
2. A durable local runtime with SQLite, native tools, and DeepSeek streaming.
3. Stage-specific multi-model routing, budgets, repair, and review.
4. Optional subscription bridges.
5. Optional server mode and remote CLI operation.
6. Web client and browser notifications.
7. Extensibility and public-release packaging.

Broad integrations are deferred until the local native vertical slice is
reliable. The project does not currently include a web UI, MCP integration,
multi-user SaaS, distributed workers, or automatic deployment.

## Current status

M0, M1, and the M1.5 interactive terminal milestone are implemented. Bare
`erka` opens a local keyboard-first conversation UI with live model output,
durable follow-up queues, conversation restore, activity and diff views,
session-only permission elevation, cancellation, and explicit interrupted-write
recovery. `erka run` uses the same application/runtime core. DeepSeek streaming,
SQLite state, and seven bounded coding tools remain the initial native vertical
slice. The standard suite includes a headless two-turn TUI fixture and makes no
network requests; a live DeepSeek fixture is opt-in.

The server/API is planned for M4, and browser access is planned for M5. No web
UI or background daemon is included today.

## Security posture

Repository contents, model output, tool output, downloaded dependencies, and
extension code are treated as untrusted input. Filesystem, process, network,
credential, and publication permissions must be enforced below the prompt layer.
Provider credentials must not enter model context, event history, or general
shell environments. `--full` is intentionally explicit because a local child
process runs with the current operating-system user's filesystem and network
access; the workspace working directory alone is not an isolation boundary.

## License

A license will be selected before the first public release. Until then, no
permission is granted to copy, modify, or redistribute this repository.
