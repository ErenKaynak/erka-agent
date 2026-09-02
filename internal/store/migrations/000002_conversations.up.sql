CREATE TABLE turns (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position > 0),
    prompt TEXT NOT NULL CHECK (length(trim(prompt)) > 0),
    run_id TEXT REFERENCES runs(id),
    status TEXT NOT NULL CHECK (status IN ('queued', 'claimed', 'running', 'completed', 'failed', 'cancelled', 'timed_out', 'waiting')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(task_id, position),
    UNIQUE(run_id)
);

CREATE INDEX turns_task_position_idx ON turns(task_id, position);
CREATE INDEX turns_queue_idx ON turns(status, created_at, position);

INSERT INTO turns(id, task_id, position, prompt, run_id, status, created_at, updated_at)
SELECT
    'turn_m1_' || tasks.id,
    tasks.id,
    1,
    tasks.objective,
    (SELECT runs.id FROM runs WHERE runs.task_id = tasks.id ORDER BY runs.created_at, runs.id LIMIT 1),
    CASE tasks.status
        WHEN 'completed' THEN 'completed'
        WHEN 'failed' THEN 'failed'
        WHEN 'cancelled' THEN 'cancelled'
        WHEN 'timed_out' THEN 'timed_out'
        WHEN 'waiting' THEN 'waiting'
        WHEN 'waiting_approval' THEN 'waiting'
        WHEN 'queued' THEN 'claimed'
        ELSE 'running'
    END,
    tasks.created_at,
    tasks.updated_at
FROM tasks;

CREATE TABLE project_leases (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    holder TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX project_leases_expiry_idx ON project_leases(expires_at);
