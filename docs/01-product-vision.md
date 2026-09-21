# Product vision and scope

## Problem

Developers increasingly use several model vendors and coding-agent products.
Each vendor CLI owns its own tools, permissions, context, session history, and
authentication. Switching products fragments memory and makes long-running work
hard to control from another device. Consumer subscriptions and usage-billed
model APIs also have different access paths.

`erka-agent` provides one developer-owned execution environment. It can run
inside a local repository or continuously on a self-hosted server. The runtime
selects different models for different stages while preserving a durable,
provider-neutral task record.

## Primary use cases

### Local coding

A developer installs one binary and runs `erka` inside a repository. The runtime
uses local files, local process isolation, local credentials, and a local state
store. No server account is required.

### Personal remote development

A developer runs `erka serve` on a 24/7 machine, registers repositories, submits
tasks from a CLI or web client, watches progress, receives notifications, reviews
diffs, and approves Git operations from another device.

### Stage-specific model selection

A strong model can create a plan, a less expensive model can implement it, a fast
model can repair validation failures, and an independent model can review the
result. Each stage has its own model, context packet, limits, and fallback list.

### Open-source provider expansion

Users can configure OpenAI-compatible endpoints without code changes. Providers
with incompatible protocols can be added through focused adapters. Local model
servers can participate through the same capability model.

## Product principles

1. **Own the runtime.** Native model providers use tools, context, policy, and
   persistence implemented by `erka`.
2. **Local first, remote capable.** Local operation is complete on its own;
   server mode adds durability and remote access rather than unlocking the core.
3. **Provider-neutral state.** A task belongs to the user, not a vendor session.
4. **Explicit economics.** Every native model run records tokens, cache usage,
   latency, and estimated or reported cost.
5. **Honest integration labels.** Native APIs and subscription bridges are
   different capabilities and appear differently in configuration and UI.
6. **Safe by construction.** Filesystem, process, network, Git, and credential
   boundaries are enforced below the prompt layer.
7. **Useful before broad.** One reliable vertical slice is more valuable than a
   large provider catalog with shallow support.

## Version-one boundaries

Version one includes a Go runtime, local CLI, optional server daemon, durable
sessions, native tools, stage routing, approvals, and a small initial provider
set. A web client may follow once the API is stable.

Version one does not target multi-tenant SaaS, distributed clusters, Kubernetes,
mobile-native apps, an extension marketplace, autonomous deployment, or perfect
feature parity with vendor CLIs.

## Success criteria

- A fresh user can install one binary and complete a real repository change.
- The same task can be resumed after the process restarts.
- Planner and implementer can use different model connections.
- A local task needs no daemon; a remote task survives client disconnection.
- Every file change and command is visible in an append-only event history.
- A provider outage can use a configured fallback without corrupting the task.
- Native providers never receive subscription credentials or unrelated secrets.

