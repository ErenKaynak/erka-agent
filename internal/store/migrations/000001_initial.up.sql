CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    root TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    objective TEXT NOT NULL,
    status TEXT NOT NULL,
    item_sequence INTEGER NOT NULL DEFAULT 0 CHECK (item_sequence >= 0),
    event_sequence INTEGER NOT NULL DEFAULT 0 CHECK (event_sequence >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX tasks_project_created_idx ON tasks(project_id, created_at);

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    stage TEXT NOT NULL,
    backend_class TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX runs_task_created_idx ON runs(task_id, created_at);

CREATE TABLE runtime_items (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    kind TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    UNIQUE(task_id, sequence)
);

CREATE INDEX runtime_items_task_sequence_idx ON runtime_items(task_id, sequence);
CREATE INDEX runtime_items_call_lookup_idx ON runtime_items(task_id, kind, sequence);

CREATE TABLE events (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT REFERENCES runs(id),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    message TEXT NOT NULL,
    data_json BLOB,
    UNIQUE(task_id, sequence)
);

CREATE INDEX events_task_sequence_idx ON events(task_id, sequence);
