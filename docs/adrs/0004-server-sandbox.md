# ADR 0004: personal-server sandbox

## Status

Accepted for M4.

## Decision

The first server is single-user and must run as a non-root OS account. Each task
uses a dedicated server-owned Git worktree below the erka state directory. Tool
execution reuses the existing canonical-workspace sandbox rooted at that
worktree. The sandbox boundary remains replaceable so namespace, OCI, or microVM
implementations can be added later without changing tools or the runtime.

## Consequences

The M4 boundary isolates tasks from the registered source checkout and from one
another, but it is not a hostile multi-tenant boundary. Server startup refuses
root execution unless a test-only dependency explicitly bypasses the check.

