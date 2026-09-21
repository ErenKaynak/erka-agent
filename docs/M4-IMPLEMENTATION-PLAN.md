# M4 implementation checkpoint

This document tracks the Optional Server Mode milestone.

## Delivery slices

1. [x] Record the server sandbox, API, authentication, and notification decisions
   in focused ADRs.
2. [x] Add forward-only SQLite storage for logical server projects, task worktrees,
   durable worker claims, scoped API-token hashes, and approvals.
3. [x] Add server-side Git worktree lifecycle management and a durable single-node
   worker that reuses the existing application session and runtime core.
4. [x] Add a clean `/v1` HTTP API for health, identity, projects, tasks, messages,
   cancellation, retry, items, events, diffs, approvals, connections, models,
   and pipelines.
5. [x] Add authenticated resumable SSE with event-ID deduplication and heartbeat
   behavior. Client disconnects must never cancel task execution.
6. [x] Add remote CLI profiles plus projects, run, watch, show, diff, cancel, retry,
   and approval commands without weakening local command behavior.
7. [ ] Add a generic webhook sink backed by a durable outbox. Delivery failure is
   independent of task success and retries are deduplicated by event ID.
8. [ ] Prove crash recovery, worker-claim expiry, worktree isolation, token scopes
   and revocation, unauthorized SSE resume, approval decisions, webhook retry,
   and a disconnected-client end-to-end task.
9. [ ] Update public documentation, run the complete verification matrix, publish
   a completion commit, and tag `milestone-m4`.

## Locked M4 decisions

- Server mode is single-user but never anonymous. API access uses opaque bearer
  tokens; only SHA-256 token hashes and scopes are stored in SQLite.
- The server API starts as a clean `/v1` contract and does not inherit the old
  AI-LXC API merely for shape compatibility.
- Every task executes in a server-owned Git worktree below the state directory.
  Local mode remains in-process and unchanged.
- The initial notification target is a generic HTTPS webhook with a durable,
  at-least-once outbox keyed by committed event ID.
- The personal-server process must run as a non-root OS user. The existing
  workspace sandbox remains the tool boundary inside an isolated task worktree;
  stronger namespace or OCI isolation stays behind the same sandbox interface.

## Exit demonstration

Start `erka serve`, submit a task through an authenticated remote CLI profile,
disconnect the client, reconnect to the task's SSE stream, review the isolated
worktree diff, and decide its durable approval. Restarting the daemon during the
run must not lose the queued task, committed events, or approval state.
