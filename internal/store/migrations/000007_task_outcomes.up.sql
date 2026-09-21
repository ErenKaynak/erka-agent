ALTER TABLE server_tasks ADD COLUMN outcome TEXT CHECK (outcome IS NULL OR outcome IN ('accepted', 'rejected'));
ALTER TABLE server_tasks ADD COLUMN result_ref TEXT;
ALTER TABLE server_tasks ADD COLUMN result_base TEXT;
ALTER TABLE server_tasks ADD COLUMN result_commit TEXT;
ALTER TABLE server_tasks ADD COLUMN finalized_at TEXT;

CREATE INDEX server_tasks_finalize_idx ON server_tasks(status, finalized_at);
