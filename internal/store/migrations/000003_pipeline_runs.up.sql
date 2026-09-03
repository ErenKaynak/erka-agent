ALTER TABLE tasks ADD COLUMN pipeline TEXT NOT NULL DEFAULT '';

ALTER TABLE runs ADD COLUMN connection TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0);
ALTER TABLE runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0);
ALTER TABLE runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0);
ALTER TABLE runs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0);
ALTER TABLE runs ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0);
ALTER TABLE runs ADD COLUMN cost_microunits INTEGER NOT NULL DEFAULT 0 CHECK (cost_microunits >= 0);
ALTER TABLE runs ADD COLUMN currency TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN failure_code TEXT NOT NULL DEFAULT '';

CREATE INDEX runs_task_stage_attempt_idx ON runs(task_id, stage, attempt, created_at);
