CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('global', 'project')),
    project_key TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'decision')),
    origin TEXT NOT NULL CHECK (origin IN ('user', 'model')),
    text TEXT NOT NULL CHECK (length(trim(text)) > 0),
    source_task TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((scope = 'global' AND project_key = '') OR (scope = 'project' AND project_key != ''))
);

CREATE INDEX memories_scope_idx ON memories(scope, project_key, updated_at);

CREATE VIRTUAL TABLE memories_fts USING fts5(text, content = 'memories', content_rowid = 'rowid');

CREATE TRIGGER memories_fts_insert AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER memories_fts_delete AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;

CREATE TRIGGER memories_fts_update AFTER UPDATE OF text ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
    INSERT INTO memories_fts(rowid, text) VALUES (new.rowid, new.text);
END;
