# erka-agent

`erka-agent` is a local-first, self-hostable coding-agent runtime written in Go.
It owns the agent loop, tools, context, sessions, routing, permissions, and event
history while treating model vendors as interchangeable intelligence providers.

The user-facing executable is `erka`:

```bash
# Work directly in the current local repository.
erka run "Fix the failing checkout tests"

# Start the same runtime as a long-running server.
erka serve

# Submit and follow work on a remote installation.
erka --profile home run --project shop "Implement the approved plan"
erka --profile home watch <task-id>
```

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
6. Web client, extensibility, and public-release packaging.

Broad integrations are deferred until the local native vertical slice is
reliable. The project does not currently include a web UI, MCP integration,
multi-user SaaS, distributed workers, or automatic deployment.

## Current status

M0 foundation work is in progress. The CLI and internal contracts are not yet a
usable coding agent. Early commands may intentionally report that their runtime
stage has not been implemented.

## Security posture

Repository contents, model output, tool output, downloaded dependencies, and
extension code are treated as untrusted input. Filesystem, process, network,
credential, and publication permissions must be enforced below the prompt layer.
Provider credentials must not enter model context, event history, or general
shell environments.

## License

A license will be selected before the first public release. Until then, no
permission is granted to copy, modify, or redistribute this repository.
