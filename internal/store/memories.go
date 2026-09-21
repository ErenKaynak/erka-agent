package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	MemoryGlobal  = "global"
	MemoryProject = "project"

	MaxMemoryTextBytes = 2000
	maxRecallTerms     = 24
)

var memoryKinds = map[string]bool{"fact": true, "preference": true, "decision": true}

// MemoryNote is one durable note shared by every model and connection. Global
// memories apply everywhere; project memories apply to one repository key.
type MemoryNote struct {
	ID         string
	Scope      string
	ProjectKey string
	Kind       string
	Origin     string
	Text       string
	SourceTask erkaruntime.TaskID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (memory MemoryNote) Validate() error {
	if memory.ID == "" || memory.CreatedAt.IsZero() || memory.UpdatedAt.IsZero() {
		return errors.New("memory requires ID and timestamps")
	}
	if memory.Scope == MemoryGlobal && memory.ProjectKey != "" || memory.Scope == MemoryProject && memory.ProjectKey == "" || memory.Scope != MemoryGlobal && memory.Scope != MemoryProject {
		return errors.New("memory scope must be global without a project or project with a project key")
	}
	if !memoryKinds[memory.Kind] {
		return fmt.Errorf("unsupported memory kind %q", memory.Kind)
	}
	if memory.Origin != "user" && memory.Origin != "model" {
		return fmt.Errorf("unsupported memory origin %q", memory.Origin)
	}
	text := strings.TrimSpace(memory.Text)
	if text == "" || len(text) > MaxMemoryTextBytes {
		return fmt.Errorf("memory text must be 1-%d bytes", MaxMemoryTextBytes)
	}
	return nil
}

// SaveMemory inserts a memory, or refreshes the timestamp of an identical one
// in the same scope so repeated saves do not accumulate duplicates.
func (store *SQLite) SaveMemory(ctx context.Context, memory MemoryNote) (MemoryNote, error) {
	memory.Text = strings.TrimSpace(memory.Text)
	if err := memory.Validate(); err != nil {
		return MemoryNote{}, err
	}
	existing, err := scanMemory(store.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE scope = ? AND project_key = ? AND text = ?`, memory.Scope, memory.ProjectKey, memory.Text))
	if err == nil {
		_, err = store.db.ExecContext(ctx, `UPDATE memories SET updated_at = ? WHERE id = ?`, formatTime(memory.UpdatedAt), existing.ID)
		existing.UpdatedAt = memory.UpdatedAt
		return existing, err
	}
	if !errors.Is(err, erkaruntime.ErrNotFound) {
		return MemoryNote{}, err
	}
	var source any
	if memory.SourceTask != "" {
		source = memory.SourceTask
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO memories(id, scope, project_key, kind, origin, text, source_task, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, memory.ID, memory.Scope, memory.ProjectKey, memory.Kind, memory.Origin, memory.Text, source, formatTime(memory.CreatedAt), formatTime(memory.UpdatedAt))
	return memory, mapInsertError(err, "memory", memory.ID)
}

func (store *SQLite) DeleteMemory(ctx context.Context, id string) error {
	result, err := store.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("memory %q: %w", id, erkaruntime.ErrNotFound)
	}
	return nil
}

// ListMemories lists global memories and, when projectKey is set, that
// project's memories, newest first.
func (store *SQLite) ListMemories(ctx context.Context, projectKey string, limit int) ([]MemoryNote, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE scope = 'global' OR (scope = 'project' AND project_key = ? AND ? != '') ORDER BY updated_at DESC, id LIMIT ?`, projectKey, projectKey, limit)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// SearchMemories ranks visible memories against free text with FTS5 BM25.
// The query is reduced to quoted word terms, so user or model text can never
// inject FTS query syntax.
func (store *SQLite) SearchMemories(ctx context.Context, projectKey, query string, limit int) ([]MemoryNote, error) {
	terms := memoryTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return []MemoryNote{}, nil
	}
	rows, err := store.db.QueryContext(ctx, `SELECT m.id, m.scope, m.project_key, m.kind, m.origin, m.text, m.source_task, m.created_at, m.updated_at
		FROM memories_fts JOIN memories m ON m.rowid = memories_fts.rowid
		WHERE memories_fts MATCH ? AND (m.scope = 'global' OR (m.scope = 'project' AND m.project_key = ? AND ? != ''))
		ORDER BY bm25(memories_fts), m.updated_at DESC LIMIT ?`, strings.Join(terms, " OR "), projectKey, projectKey, limit)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// Preferences returns user-origin global preferences, which apply to every
// task regardless of wording.
func (store *SQLite) Preferences(ctx context.Context, limit int) ([]MemoryNote, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE scope = 'global' AND kind = 'preference' AND origin = 'user' ORDER BY updated_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

func memoryTerms(query string) []string {
	seen := make(map[string]bool)
	terms := make([]string, 0, maxRecallTerms)
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(word)) < 3 || stopWords[word] || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, `"`+word+`"`)
		if len(terms) == maxRecallTerms {
			break
		}
	}
	return terms
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true, "from": true, "into": true,
	"make": true, "use": true, "add": true, "fix": true, "all": true, "are": true, "was": true, "should": true,
	"please": true, "can": true, "you": true, "our": true, "its": true, "then": true, "when": true, "not": true,
}

const memoryColumns = `id, scope, project_key, kind, origin, text, source_task, created_at, updated_at`

func collectMemories(rows *sql.Rows) ([]MemoryNote, error) {
	defer rows.Close()
	result := make([]MemoryNote, 0)
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, memory)
	}
	return result, rows.Err()
}

func scanMemory(row rowScanner) (MemoryNote, error) {
	var memory MemoryNote
	var source sql.NullString
	var created, updated string
	if err := row.Scan(&memory.ID, &memory.Scope, &memory.ProjectKey, &memory.Kind, &memory.Origin, &memory.Text, &source, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryNote{}, fmt.Errorf("memory: %w", erkaruntime.ErrNotFound)
		}
		return MemoryNote{}, err
	}
	memory.SourceTask = erkaruntime.TaskID(source.String)
	var err error
	if memory.CreatedAt, err = parseTime(created); err == nil {
		memory.UpdatedAt, err = parseTime(updated)
	}
	return memory, err
}
