CREATE TABLE server_projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    repository_root TEXT NOT NULL UNIQUE,
    default_ref TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE server_tasks (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    server_project_id TEXT NOT NULL REFERENCES server_projects(id),
    worktree_root TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'waiting', 'completed', 'failed', 'cancelled')),
    claim_holder TEXT,
    claim_expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((status = 'running' AND claim_holder IS NOT NULL AND claim_expires_at IS NOT NULL) OR status != 'running')
);

CREATE INDEX server_tasks_queue_idx ON server_tasks(status, claim_expires_at, created_at);
CREATE INDEX server_tasks_project_idx ON server_tasks(server_project_id, updated_at);

CREATE TABLE approvals (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES server_tasks(task_id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    message TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    actor TEXT,
    requested_at TEXT NOT NULL,
    expires_at TEXT,
    decided_at TEXT
);

CREATE INDEX approvals_task_idx ON approvals(task_id, requested_at);
CREATE INDEX approvals_pending_idx ON approvals(status, expires_at, requested_at);

CREATE TABLE server_api_tokens (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    scopes_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);

CREATE INDEX server_api_tokens_active_idx ON server_api_tokens(revoked_at, created_at);

