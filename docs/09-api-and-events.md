# Server API and event protocol

## Design goals

The server API is an optional transport over the same application services used
by local CLI mode. It supports durable task submission, reconnectable progress,
approvals, cancellation, diffs, and follow-up messages.

The API is versioned independently from the binary release. JSON fields use a
consistent naming convention, additive changes are tolerated, and every error
has a machine-readable code plus a user-facing message.

## Initial resource outline

```text
GET    /v1/health
GET    /v1/me

GET    /v1/projects
POST   /v1/projects
GET    /v1/projects/{id}
PATCH  /v1/projects/{id}

GET    /v1/tasks
POST   /v1/tasks
GET    /v1/tasks/{id}
POST   /v1/tasks/{id}/messages
POST   /v1/tasks/{id}/cancel
POST   /v1/tasks/{id}/retry
GET    /v1/tasks/{id}/items
GET    /v1/tasks/{id}/events
GET    /v1/tasks/{id}/events/stream
GET    /v1/tasks/{id}/diff

GET    /v1/approvals
POST   /v1/approvals/{id}/decision

GET    /v1/connections
GET    /v1/models
GET    /v1/pipelines
```

Credential submission endpoints require additional protection and never return
credential values. Device authentication is represented as a short-lived login
flow resource rather than a long-running HTTP request.

## Event envelope

```json
{
  "id": "evt_...",
  "sequence": 42,
  "taskId": "task_...",
  "runId": "run_...",
  "type": "tool.completed",
  "time": "2026-09-02T12:00:00Z",
  "message": "Tests completed",
  "data": {
    "tool": "run_validation",
    "exitCode": 0
  }
}
```

Events expose summaries, not unrestricted raw provider objects. Large outputs are
stored as bounded artifacts and referenced by ID. Hidden reasoning is never an
event payload.

## Streaming

Server-Sent Events are sufficient for ordered task activity. Clients resume with
the last committed sequence or event ID and deduplicate by event ID. Heartbeats
keep intermediaries from closing idle connections. A terminal feature may use a
separate WebSocket endpoint later; it is not required for the first server
milestone.

## Task states

```text
queued → preparing → planning → coding → validating → reviewing → completed
                                 │            │
                                 └─waiting────┘

Any active state may transition to failed, cancelled, timed_out, or
waiting_approval when the operation permits it.
```

Stage runs carry their own attempt and status records. A task status summarizes
the active stage rather than replacing detailed run history.

## Notifications

Notification sinks consume committed terminal, approval, and attention-required
events. Initial support should be a generic webhook and web push or an
open-source-friendly notification target. Notification failure never changes the
task result and is retried independently.

