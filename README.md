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

On first launch, erka asks for the selected provider credentials in masked
terminal fields and saves them in the operating-system credential store. If the
credential store is unavailable, a key remains in memory for that process only.
Environment credentials always take precedence:

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

Typing `/` in the composer shows matching local commands; press `Tab` to
complete the first match. Commands are handled by the TUI and are never sent to
the model:

- `/help`: show keyboard and command help.
- `/new`: start a new conversation.
- `/sessions`: open the conversation picker.
- `/open <task-id>`: open a conversation by its durable ID.
- `/diff`: inspect the current working-tree diff.
- `/permissions`: open the session permission confirmation.
- `/full`: request full command execution with confirmation.
- `/workspace`: immediately return to workspace-only execution.
- `/cancel`: confirm cancellation of the active turn.
- `/status`: show the current task, provider, state, and permission mode.
- `/provider`: show native connection and subscription bridge status.
- `/model`: discover models and open the searchable per-stage model picker.
- `/quit`: confirm exit.

Use `//text` to send a model prompt that begins with a literal `/`. Help, diff,
permission, recovery, cancellation, and exit dialogs are bounded to the current
terminal viewport; `Esc` closes a dialog without acting.

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
the built-in defaults. This minimal configuration runs one DeepSeek coding
stage:

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
`https://api.deepseek.com`; `--provider-base-url` (or `ERKA_PROVIDER_BASE_URL`)
selects a compatible test endpoint, and `--api-key-env` (or `ERKA_API_KEY_ENV`)
overrides the credential environment name. Secrets do not belong in YAML—
`ERKA_DEEPSEEK_API_KEY` is read by the trusted provider adapter and excluded
from tool subprocess environments.

### Fixed multi-model pipelines

M2 can assign different native connections and models to planning, coding,
validation, repair, and review. Routes are fixed and deterministic: erka tries
each stage's candidates in the listed order and only falls back after a
retryable failure that happened before any mutating tool ran.

```yaml
version: 1
pipeline: default

connections:
  planner:
    provider: openai
    api_key_env: OPENAI_API_KEY
    models:
      gpt-model-id:
        capabilities: [tools, structured_output]
        context_tokens: 128000
  coder:
    provider: deepseek
    api_key_env: ERKA_DEEPSEEK_API_KEY

pipelines:
  default:
    routing: fixed
    max_repair_attempts: 1
    stages:
      planning:
        candidates: [planner/gpt-model-id]
        required_capabilities: [structured_output]
        budget:
          max_input_tokens: 20000
          max_output_tokens: 4000
          max_iterations: 3
          timeout: 5m
      coding:
        candidates: [coder/deepseek-v4-flash]
      validation:
        candidates: [coder/deepseek-v4-flash]
      repair:
        candidates: [coder/deepseek-v4-flash]
      review:
        candidates: [planner/gpt-model-id]

permissions:
  allow_full_execution: false
```

Replace `gpt-model-id` with an OpenAI model available to your API account, set
both environment variables, then launch either interface:

```bash
./erka --pipeline default
./erka --pipeline default run --full "Fix the failing checkout tests"
```

The `pipeline` field selects a configured pipeline; `--pipeline` and
`ERKA_PIPELINE` override it. Planning, validation, and review receive read-only
tools. Coding and repair may use workspace mutations, while arbitrary process
execution still requires `--full` or an in-TUI confirmation. Each stage stores
its own run, checkpoint, token usage, configured cost, and terminal status in
SQLite. Model-to-model handoff uses bounded durable checkpoints instead of
replaying another provider's full transcript.

The validation stage must finish with a single JSON object, optionally inside
one Markdown code fence:

```json
{"status": "pass", "failures": [], "notes": "go test ./... passed"}
```

`status` is `pass` or `fail`, and `pass` requires an empty `failures` list. The
report is checked against a JSON Schema and stored in the stage checkpoint.
Any other output counts as a failed validation and goes to repair (or fails the
task when the repair budget is spent). Checkpoints recorded before structured
validation keep their original verdict when such a task is resumed.

Optional model pricing uses integer microunits per million tokens:

```yaml
models:
  gpt-model-id:
    input_cost_per_million_microunits: 1000000
    output_cost_per_million_microunits: 2000000
    currency: USD
```

Cost limits can then be set with `max_cost_microunits` and `currency` inside a
stage budget. These values are configuration supplied by the operator; erka
does not fetch changing prices from a provider.

### Subscription bridges

M3 can use an installed vendor CLI as an explicitly labeled subscription
bridge for planning or review. Bridge credentials remain in the vendor's own
credential store and are never copied into erka configuration, SQLite, model
context, or native-provider subprocess environments.

Sign in to Codex with ChatGPT subscription authentication through the official
device-code flow, then inspect bridge availability:

```bash
erka auth login codex --device
erka auth status
erka providers
```

Inside the TUI, the `/provider` command shows the same native connection and
subscription bridge status with connection guidance. `/model` queries only the
configured connections, merges their live catalogs with configured model IDs,
and lets you choose planning, coding, validation, repair, and review models
independently. The selection lasts for the current erka process and does not
rewrite configuration.

The same read-only discovery is available for scripts and diagnostics:

```bash
erka models
```

Codex models come from the signed-in CLI's app-server catalog, OpenCode models
come from `opencode models --pure`, and native DeepSeek/OpenAI models come from
their authenticated model-list endpoints. Discovery does not run an inference
request. If a live catalog is unavailable, configured models remain visible and
the failing connection is reported as a warning.

OpenCode authentication can be delegated with `erka auth login opencode`. Both
CLIs are optional runtime discoveries; erka does not download, link, or depend
on either executable.

Configure a bridge connection with `class: subscription_bridge`. `default`
lets Codex select the model available to the signed-in account; an explicit
model may be used instead. OpenCode model IDs may retain their
`provider/model` form after the erka connection name.

```yaml
version: 1
pipeline: subscription-plan

connections:
  chatgpt:
    class: subscription_bridge
    provider: codex
    models:
      default:
        capabilities: [tools, structured_output]
  coder:
    provider: deepseek
    api_key_env: ERKA_DEEPSEEK_API_KEY

pipelines:
  subscription-plan:
    routing: fixed
    stages:
      planning:
        candidates: [chatgpt/default]
        required_capabilities: [structured_output]
        budget:
          max_input_tokens: 20000
          max_output_tokens: 4000
          timeout: 10m
      coding:
        candidates: [coder/deepseek-v4-flash]
```

Codex runs non-interactively with its read-only sandbox, ephemeral vendor
session, JSONL events, and a strict checkpoint output schema. OpenCode runs its
JSON stream through a deny-by-default plan profile with project configuration
disabled. Subscription bridges are rejected for coding, validation, and repair
stages.

## Server mode

`erka serve` runs the same runtime as a personal HTTP server so tasks keep
running while you are away and can be controlled from another machine. It is
optional: nothing else in erka needs a daemon.

```bash
./erka server token create laptop --scope read,write,approve
./erka serve --listen 127.0.0.1:7432
```

- Plain HTTP only binds to loopback addresses. To listen on any other address,
  pass `--tls-cert` and `--tls-key`. erka refuses to serve as the root OS user.
- Every endpoint except `GET /v1/health` needs a bearer token. Tokens are
  scoped to `read`, `write`, `approve`, or `admin`; the secret is printed once
  and can be revoked with `erka server token revoke <token-id>`.
- Each remote task runs in its own Git worktree on the server, so concurrent
  tasks never edit the repository checkout directly.
- `GET /v1/tasks/{id}/events/stream` is a Server-Sent Events stream of
  committed task events. Reconnecting with `Last-Event-ID` resumes after the
  last delivered event.
- Queued tasks are claimed by a worker with an expiring lease
  (`--claim-duration`), so a task whose worker stopped can be claimed again.

From another machine, save a profile once. The token is read from an
environment variable and stored in the operating-system credential store:

```bash
export ERKA_SERVER_TOKEN='token-from-server'
./erka profile add home https://erka.example.internal:7432
./erka --profile home projects add app /srv/repos/app
./erka --profile home run --project <project-id> "Fix the failing checkout tests"
./erka --profile home watch <task-id>
./erka --profile home approvals
./erka --profile home approve <approval-id>
```

`show`, `diff`, `cancel`, `retry`, and `reject` work the same way, and `watch
--after <event>` continues from a known event. Remote `run` does not accept
`--resume`; use `watch` or `retry` instead.

Prefer reaching the server over a private network such as Tailscale or
WireGuard instead of exposing it to the public internet.

## Product promise

- Run without a server on macOS, Linux, or Windows.
- Run continuously on a self-hosted server and control it from another device.
- Use different models for planning, implementation, repair, and review.
- Keep coding tools and durable task history under the user's control.
- Support native model APIs and clearly labeled subscription bridges.
- Ship as open source without a mandatory hosted service.

## Runtime model

Native DeepSeek and OpenAI API connections use the `erka` tool loop end to end.
OpenAI uses the Responses streaming API; DeepSeek uses its streaming chat API.
Subscription integrations are explicit compatibility bridges and are never
represented as native model APIs. Local commands use the same application and
runtime core as optional server mode, but do not require a daemon or hidden HTTP
server.

## Roadmap

Development proceeds through demonstrable milestones:

1. Go foundation, command contracts, configuration, tests, and release builds.
2. A durable local runtime with SQLite, native tools, and DeepSeek streaming.
3. Stage-specific multi-model routing, budgets, repair, and review.
4. Optional subscription bridges.
5. Optional server mode and remote CLI operation (implemented).
6. Web client and browser notifications.
7. Extensibility and public-release packaging.

Broad integrations are deferred until the local native vertical slice is
reliable. The project does not currently include a web UI, MCP integration,
multi-user SaaS, distributed workers, or automatic deployment.

## Current status

M0, M1, M1.5, M2, M3, and M4 are implemented. Bare `erka` opens a local keyboard-first
conversation UI with live model output, durable follow-up queues, conversation
restore, activity and diff views, session-only permission elevation,
cancellation, and explicit interrupted-write recovery. `erka run` uses the same
application/runtime core.

Configured fixed pipelines now run planning, coding, validation, bounded repair,
and review through native DeepSeek and OpenAI connections. Stage checkpoints,
fallback attempts, token/cost accounting, and restart state are durable. The
standard suite includes headless TUI and two-provider protocol fixtures and
makes no network requests; a live DeepSeek fixture remains opt-in.

Optional Codex and OpenCode subscription bridges provide bounded read-only
planning/review stages, canonical checkpoint translation, capability and login
reporting, and deterministic fallback. Codex device authentication is delegated
to the official CLI; erka never handles the resulting ChatGPT credentials.

Optional server mode (`erka serve`) provides an authenticated HTTP API with
scoped bearer tokens, SSE task events with `Last-Event-ID` resume, remote
approvals, and per-task Git worktrees. Remote profiles let the CLI create,
watch, approve, cancel, and retry tasks on that server. See
[Server mode](#server-mode). Browser access is planned for M5; no web UI is
included today, and erka never starts a background daemon on its own.

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
