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
- `Ctrl+D`: open the colored, scrollable working-tree diff.
- `Ctrl+T`: show or hide tool output under each tool step.
- `PgUp`/`PgDn`: scroll the transcript; `Ctrl+G` jumps back to the latest output.
- `Shift+Tab`: cycle auto → plan (read-only, purple) → god mode (full access, red).
- `Ctrl+P`: confirm workspace/full execution for this process.
- `Esc`: close an overlay or confirm cancellation of an active turn.
- `Ctrl+C`: clear the input; press it again within two seconds to quit. Queued prompts remain durable.
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
- `/plan`: switch to read-only plan mode.
- `/auto`: return to auto mode.
- `/full`: request full command execution with confirmation.
- `/workspace`: immediately return to workspace-only execution.
- `/cancel`: confirm cancellation of the active turn.
- `/status`: show the current task, provider, state, and permission mode.
- `/provider`: show native connection and subscription bridge status.
- `/model`: discover models and open the per-stage model picker, grouped by stage with the current model marked.
- `/quit`: exit immediately.

The TUI shows each tool call as one line with its target and outcome, a live
status line with the current step, elapsed time, and token usage, and a
filterable conversation picker. When erka asks a question, answer in the
composer or press an option number; `Esc` lets it decide. Commands are offered
with per-call consent: `y` allows once, `a` allows that tool for the session,
and `n` denies. Full access (`Ctrl+P` or `--full`) skips these prompts.

Commands the model runs are confined by an OS sandbox (`sandbox-exec` on macOS,
bubblewrap on Linux): they can write only inside the repository, a private
temporary directory, and build caches; they cannot reach the network (macOS still allows the machine's own services;
Linux isolates the network entirely); credential folders such as `~/.ssh` and `~/.aws` are unreadable; and
`.git/config` and `.git/hooks` are read-only so nothing can plant code that
runs later outside the sandbox. God mode is the only unsandboxed mode.

```yaml
sandbox:
  mode: auto          # auto | required | off
  network: false      # allow sandboxed commands to reach the network
  writable_paths: []  # extra writable directories, for example ~/.gradle
```

`/status` shows the active policy. On platforms without a sandbox backend,
`auto` runs commands unconfined and the header says `no sandbox`; `required`
refuses to run them instead.

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

Use `--cwd <path>` to select another repository and `-s <session>` (or
`--task <id>`) to reopen a conversation; a unique prefix of the ID is enough.
When you quit, erka prints the exact `erka -s …` command for the session you left. Each scripted run prints its durable task ID
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

### Complexity-tiered routing

`routing: tiered` classifies each request as `trivial`, `standard`, or
`complex` and runs only that tier's stages, so a typo fix does not pay for
planning, validation, and review. By default trivial runs coding only,
standard adds validation and repair, and complex runs every configured stage.
A tier may also swap in cheaper candidates for a stage:

```yaml
pipelines:
  smart:
    routing: tiered
    triage:
      candidate: coder/deepseek-v4-flash   # optional; omit to use the free local heuristic
    tiers:
      trivial:
        candidates:
          coding: [coder/deepseek-v4-flash]
    stages:
      planning: {candidates: [chatgpt/default]}
      coding: {candidates: [coder/deepseek-v4-pro]}
      validation: {candidates: [coder/deepseek-v4-flash]}
      repair: {candidates: [coder/deepseek-v4-pro]}
      review: {candidates: [chatgpt/default]}
```

The decision is recorded as a durable `task.triaged` event, so a resumed task
keeps the stages it started with. A model selected with `/model` still takes
precedence over a tier's candidates.

### Long-term memory

Memory is stored in erka's own database and recalled into every stage's
context, so every connected model and subscription bridge sees the same notes.
Global memories apply everywhere; project memories apply to one repository and
are shared by its server task worktrees.

```bash
erka memory add --global "Prefer table-driven Go tests"
erka memory add --kind decision "All SQLite access goes through internal/store"
erka memory list
erka memory search "how are tests written"
erka memory rm <memory-id>
```

Models get `memory_search` in every stage and `memory_save` for project notes.
Model-saved notes are labeled unverified; only you can add global memory.

## Remote operation over a VPN

Run the server from the repository you want to work on:

```bash
erka serve
```

With no flags it listens on this computer and on your Tailscale or WireGuard
address (never a plain LAN address), registers the current repository, and
prints a QR code. Scan it with your phone, which must be on the same VPN, and
the web app opens already paired; there are no tokens to copy. The one-time
code expires after ten minutes. Pair another device later with
`erka server pair`. Commands the model runs are sandboxed and ask you first.

Everything below is optional configuration.

`erka serve` runs tasks in isolated worktrees and can be driven from a laptop
CLI profile or a phone. On a WireGuard or Tailscale network, listen on the
server's VPN address and restrict clients to the VPN range; the tunnel encrypts
traffic and bearer tokens remain required:

```yaml
permissions:
  approve_commands: true        # expose run_command, but ask before each call
server:
  public_url: http://100.101.102.103:7432
  trusted_cidrs: [100.64.0.0/10]
  notifications:
    - type: ntfy
      url: https://ntfy.your-vpn/erka
      token_env: NTFY_TOKEN
      action_token_env: ERKA_APPROVE_TOKEN   # optional approve/deny buttons
  previews:
    bind_address: 100.101.102.103
    commands:
      dev: [npm, run, dev, --, --hostname, "{host}", --port, "{port}"]
```

```bash
erka serve --listen 100.101.102.103:7432
erka server token create phone --scope read,write,approve
```

Open `http://<vpn-address>:7432/app/` on the phone, paste the token, and add
it to the home screen. The app shows pending questions and permission requests,
live task activity, diffs, previews, and memory.

- **Questions and permissions.** Native stages can call `ask_user`, and with
  `approve_commands` each `run_command` asks first. The task parks durably
  without holding a worker, then resumes when you answer from the app, a
  notification button, or `erka remote answer <approval-id> <text>`,
  `erka remote approve <id> [--always]`, or `erka remote reject <id> --note`.
- **Reviewing results.** A finished task waits for review. Accepting commits
  its worktree to the branch `erka/<task>` in the server's repository and
  removes the worktree; rejecting removes it but keeps the changes at
  `refs/erka/rejected/<task>`. Repository hooks never run during this step,
  and `erka remote show <task>` prints the branch.
- **Notifications.** Questions, permission requests, completions, and failures
  go to ntfy or a signed generic webhook through a durable outbox. Delivery
  is retried with backoff and never affects task state. The ntfy action token
  is sent to the ntfy server, so configure it only for a self-hosted instance.
- **Previews.** `erka preview <task-id> dev` (or the app's Previews tab)
  starts a configured preset in the task worktree on the VPN address and prints
  its URL. Only presets from the server's YAML can run. For projects on your
  own machine, bind the dev server to all interfaces, for example
  `next dev -H 0.0.0.0`, and open `http://<laptop-vpn-address>:3000`.

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
5. Optional server mode and remote CLI operation.
6. Web client and browser notifications.
7. Extensibility and public-release packaging.

Broad integrations are deferred until the local native vertical slice is
reliable. The project does not currently include MCP integration, multi-user
SaaS, distributed workers, or automatic deployment.

## Current status

M0, M1, M1.5, M2, and M3 are implemented. Bare `erka` opens a local keyboard-first
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

The optional `erka serve` daemon provides the authenticated `/v1` API, durable
worker, resumable SSE, isolated task worktrees, remote CLI profiles, mid-run
questions and permission requests, ntfy and webhook notifications, VPN-only
serving, task previews, and a mobile web client at `/app/`. Tiered routing and
cross-model long-term memory work in both local and server mode. The local
TUI answers questions and command permissions inline.

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
