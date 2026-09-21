ALTER TABLE approvals ADD COLUMN tool TEXT;
ALTER TABLE approvals ADD COLUMN details_json BLOB;
ALTER TABLE approvals ADD COLUMN options_json BLOB;
ALTER TABLE approvals ADD COLUMN response TEXT;
ALTER TABLE approvals ADD COLUMN scope TEXT NOT NULL DEFAULT 'once' CHECK (scope IN ('once', 'task'));

CREATE INDEX approvals_grant_idx ON approvals(task_id, kind, tool, status, scope);

CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    task_id TEXT NOT NULL,
    approval_id TEXT,
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    options_json BLOB,
    created_at TEXT NOT NULL
);

CREATE INDEX notifications_created_idx ON notifications(created_at, id);

CREATE TABLE notification_deliveries (
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    target TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT NOT NULL,
    delivered_at TEXT,
    last_error TEXT,
    PRIMARY KEY (notification_id, target)
);
