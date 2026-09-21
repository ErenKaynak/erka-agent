# System architecture

## Top-level design

```text
                  Clients
        local CLI · remote CLI · web UI
                       │
       ┌───────────────┴────────────────┐
       │                                │
       ▼                                ▼
 Embedded local mode              Server API mode
       │                                │
       └───────────────┬────────────────┘
                       ▼
              Application services
       tasks · projects · approvals · events
                       │
                       ▼
                 Runtime engine
       context · routing · loop · checkpoints
              │                    │
              ▼                    ▼
       Execution backends        Tool registry
     native models / bridges    files / shell / git
              │                    │
              └─────────┬──────────┘
                        ▼
                 Sandbox backend
                        │
                        ▼
             repository or worktree
```

Local and server modes call the same application services. Local mode wires them
directly in-process. Remote mode exposes them through a versioned HTTP and event
API. Local mode must not start a hidden HTTP server merely to reuse code.

## Core boundaries

### ModelProvider

A native model protocol adapter. It receives provider-neutral input items and
tools, then streams provider-neutral output items such as text, tool calls,
usage, and errors. It does not execute tools.

Initial implementations:

- DeepSeek native API.
- OpenAI Responses API.
- Configurable OpenAI-compatible Chat Completions.

Later implementations may include Anthropic Messages, local Ollama, or vLLM.

### ExecutionBackend

Runs one stage and emits canonical runtime events. The normal implementation is
`NativeBackend`, which combines a `ModelProvider` with the `erka` loop and tools.
A `SubscriptionBridge` can instead delegate a stage to an officially supported
vendor agent runtime.

This distinction prevents a consumer subscription bridge from pretending to be
a raw model endpoint.

### Tool

A typed operation with a JSON Schema input, policy metadata, executor, output
limit, and audit behavior. Tools have no provider-specific representation; model
adapters translate their schemas at the wire boundary.

### SandboxBackend

Creates the execution boundary for filesystem and process tools. The first
personal-use implementation may use a dedicated OS user and a scoped repository.
The interface must allow stronger namespace or OCI isolation later.

### Store

Persists tasks, runs, ordered runtime items, events, approvals, checkpoints,
connections, and usage. SQLite is the initial local and single-user server store.
Storage details must not leak into runtime logic.

### EventSink

Receives ordered canonical events. Local CLI rendering, persistent storage, SSE,
notifications, and future telemetry are separate consumers.

## Go deliverables

The preferred initial layout is:

```text
cmd/erka/                 command entry point
internal/app/             use cases shared by local and server modes
internal/runtime/         native agent engine and canonical items
internal/routing/         pipeline resolution and optional LLM router
internal/providers/       native model protocol adapters
internal/bridges/         optional vendor subscription bridges
internal/tools/           built-in coding tools
internal/sandbox/         execution isolation implementations
internal/store/           SQLite and migrations
internal/server/          HTTP, SSE, auth, and remote task worker
internal/notify/          notification sinks
```

One `erka` binary should expose both local and server commands. Packages may be
made public only after an external integration needs a stable Go API.

## Compatibility strategy

The previous AI-LXC implementation is a behavioral reference for task states,
worktrees, approvals, terminal streaming, and frontend needs. It is not a module
dependency. The new API may preserve useful response shapes, but new runtime and
event schemas should be designed without carrying provider-normalization debt.

