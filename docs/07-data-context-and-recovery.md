# Data, context, and recovery

## Durable domain model

### Project

Repository identity, default branch, instructions path, validation commands,
permission policy, and local or server checkout metadata.

### Task

The user-owned long-lived objective. A task is provider-neutral and can contain
many stages and runs.

### Run

One execution attempt for one stage. It records backend class, connection, model,
limits, status, timing, provider request IDs, and usage totals.

### RuntimeItem

An ordered canonical item: message, tool call, tool result, usage, error, or
checkpoint. Tool calls and results retain stable call IDs.

### Event

An append-only user-facing state transition with a monotonically increasing
sequence number. Events support local rendering, remote resume, audit, and
notifications.

### Checkpoint

A structured handoff record containing:

- current objective and constraints;
- approved plan;
- decisions already made;
- files changed or considered relevant;
- current diff summary;
- validation results;
- unresolved problems;
- suggested next action.

### Approval

A durable request for an operation such as network access, dependency install,
commit, push, pull request, or deployment. It has expiry, actor, decision, and
execution status.

## Context compiler

The compiler creates a new immutable context snapshot for every model request.
Inputs may include project instructions, scoped instruction files, task
checkpoint, recent canonical items, relevant file excerpts, Git state, and the
stage-specific objective.

Context is selected by usefulness, not only chronology. Old raw command output is
replaced by bounded summaries. Stable prefixes should remain stable to improve
provider prompt-cache hits. The compiler records why each component was included
and its estimated token size.

Model context windows are upper limits, not targets. Each model profile defines a
smaller operational budget with reserved space for output and tool results.

## Crash consistency

The system persists a tool call before execution and persists its result before
submitting the next model request. On restart:

- read-only calls may be repeated when explicitly marked safe;
- mutating calls require an idempotency key or reconciliation;
- an unknown mutating result pauses for inspection;
- completed results are never executed again merely because an event was not
  delivered;
- provider retries reuse documented idempotency mechanisms when available.

Task, run, and event state transitions occur in transactions. Notifications are
derived from committed events and may be delivered at least once, so notification
sinks must deduplicate by event ID.

## Initial storage

SQLite is appropriate for local operation and a personal single-node server.
Migrations are embedded in the binary. The store interface should allow a future
server database without weakening SQLite support or forcing local users to run a
database service.

Retention is configurable separately for events, raw diagnostics, worktrees, and
checkpoints. Removing old diagnostics must not make the visible task history
incoherent.

