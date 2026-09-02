package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	modernsqlite "modernc.org/sqlite"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const timestampLayout = time.RFC3339Nano

// SQLite is the durable local Store implementation.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite creates or opens a database, applies embedded migrations, and
// verifies that foreign-key enforcement is active for every connection.
func OpenSQLite(ctx context.Context, filename string) (*SQLite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(filename) {
		return nil, fmt.Errorf("SQLite path must be absolute: %q", filename)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return nil, fmt.Errorf("create SQLite directory: %w", err)
	}

	dsn := sqliteDSN(filename)
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("connect SQLite: %w", err)
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect SQLite file: %w", err)
	}
	if err := migrateSQLite(database); err != nil {
		_ = database.Close()
		return nil, err
	}

	return &SQLite{db: database}, nil
}

func sqliteDSN(filename string) string {
	location := &url.URL{Scheme: "file", Path: filepath.ToSlash(filename)}
	query := location.Query()
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")
	location.RawQuery = query.Encode()
	return location.String()
}

func migrateSQLite(database *sql.DB) error {
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	driver, err := migratesqlite.WithInstance(database, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("initialize SQLite migrations: %w", err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "erka", driver)
	if err != nil {
		return fmt.Errorf("create SQLite migrator: %w", err)
	}
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply SQLite migrations: %w", err)
	}
	return nil
}

func (store *SQLite) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

func (store *SQLite) CreateProject(ctx context.Context, project erkaruntime.Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateProject(project); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO projects(id, root, created_at) VALUES(?, ?, ?)`,
		project.ID, project.Root, formatTime(project.CreatedAt),
	)
	return mapInsertError(err, "project", string(project.ID))
}

func (store *SQLite) CreateTask(ctx context.Context, task erkaruntime.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateTask(task); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO tasks(id, project_id, objective, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		task.ID, task.ProjectID, task.Objective, task.Status,
		formatTime(task.CreatedAt), formatTime(task.UpdatedAt),
	)
	return mapInsertError(err, "task", string(task.ID))
}

func (store *SQLite) CreateRun(ctx context.Context, run erkaruntime.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := erkaruntime.ValidateRun(run); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO runs(
			id, task_id, stage, backend_class, provider, model, status,
			created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.TaskID, run.Stage, run.BackendClass, run.Provider, run.Model,
		run.Status, formatTime(run.CreatedAt), formatTime(run.UpdatedAt),
	)
	return mapInsertError(err, "run", string(run.ID))
}

func (store *SQLite) LoadProject(ctx context.Context, projectID erkaruntime.ProjectID) (erkaruntime.Project, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.Project{}, err
	}
	var project erkaruntime.Project
	var createdAt string
	err := store.db.QueryRowContext(ctx, `
		SELECT id, root, created_at FROM projects WHERE id = ?`, projectID,
	).Scan(&project.ID, &project.Root, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return erkaruntime.Project{}, fmt.Errorf("project %q: %w", projectID, erkaruntime.ErrNotFound)
	}
	if err != nil {
		return erkaruntime.Project{}, fmt.Errorf("load project %q: %w", projectID, err)
	}
	project.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return erkaruntime.Project{}, fmt.Errorf("load project %q creation time: %w", projectID, err)
	}
	return project, nil
}

func (store *SQLite) LoadTask(ctx context.Context, taskID erkaruntime.TaskID) (erkaruntime.TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.TaskSnapshot{}, err
	}
	var snapshot erkaruntime.TaskSnapshot
	var status, createdAt, updatedAt string
	err := store.db.QueryRowContext(ctx, `
		SELECT id, project_id, objective, status, created_at, updated_at,
		       item_sequence, event_sequence
		FROM tasks WHERE id = ?`, taskID,
	).Scan(
		&snapshot.ID, &snapshot.ProjectID, &snapshot.Objective, &status,
		&createdAt, &updatedAt, &snapshot.LastItemSequence, &snapshot.LastEventSequence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("task %q: %w", taskID, erkaruntime.ErrNotFound)
	}
	if err != nil {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("load task %q: %w", taskID, err)
	}
	snapshot.Status = erkaruntime.TaskStatus(status)
	if snapshot.CreatedAt, err = parseTime(createdAt); err != nil {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("load task %q creation time: %w", taskID, err)
	}
	if snapshot.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("load task %q update time: %w", taskID, err)
	}
	return snapshot, nil
}

func (store *SQLite) LoadRun(ctx context.Context, runID erkaruntime.RunID) (erkaruntime.Run, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.Run{}, err
	}
	var run erkaruntime.Run
	var status, createdAt, updatedAt string
	err := store.db.QueryRowContext(ctx, `
		SELECT id, task_id, stage, backend_class, provider, model, status,
		       created_at, updated_at
		FROM runs WHERE id = ?`, runID,
	).Scan(
		&run.ID, &run.TaskID, &run.Stage, &run.BackendClass, &run.Provider,
		&run.Model, &status, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return erkaruntime.Run{}, fmt.Errorf("run %q: %w", runID, erkaruntime.ErrNotFound)
	}
	if err != nil {
		return erkaruntime.Run{}, fmt.Errorf("load run %q: %w", runID, err)
	}
	run.Status = erkaruntime.RunStatus(status)
	if run.CreatedAt, err = parseTime(createdAt); err != nil {
		return erkaruntime.Run{}, fmt.Errorf("load run %q creation time: %w", runID, err)
	}
	if run.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return erkaruntime.Run{}, fmt.Errorf("load run %q update time: %w", runID, err)
	}
	return run, nil
}

func (store *SQLite) LoadItems(ctx context.Context, taskID erkaruntime.TaskID, after uint64) ([]erkaruntime.RuntimeItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := store.LoadTask(ctx, taskID); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
		SELECT id, run_id, sequence, kind, occurred_at, payload_json
		FROM runtime_items
		WHERE task_id = ? AND sequence > ?
		ORDER BY sequence`, taskID, after)
	if err != nil {
		return nil, fmt.Errorf("load task %q items: %w", taskID, err)
	}
	defer rows.Close()

	items := make([]erkaruntime.RuntimeItem, 0)
	for rows.Next() {
		item, scanErr := scanItem(rows, taskID)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load task %q items: %w", taskID, err)
	}
	return items, nil
}

func (store *SQLite) LoadEvents(ctx context.Context, taskID erkaruntime.TaskID, after uint64) ([]erkaruntime.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := store.LoadTask(ctx, taskID); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
		SELECT id, run_id, sequence, type, occurred_at, message, data_json
		FROM events
		WHERE task_id = ? AND sequence > ?
		ORDER BY sequence`, taskID, after)
	if err != nil {
		return nil, fmt.Errorf("load task %q events: %w", taskID, err)
	}
	defer rows.Close()

	events := make([]erkaruntime.Event, 0)
	for rows.Next() {
		var event erkaruntime.Event
		var runID sql.NullString
		var occurredAt string
		var data []byte
		event.TaskID = taskID
		if err := rows.Scan(&event.ID, &runID, &event.Sequence, &event.Type, &occurredAt, &event.Message, &data); err != nil {
			return nil, fmt.Errorf("scan task %q event: %w", taskID, err)
		}
		if runID.Valid {
			event.RunID = erkaruntime.RunID(runID.String)
		}
		if event.Time, err = parseTime(occurredAt); err != nil {
			return nil, fmt.Errorf("scan task %q event time: %w", taskID, err)
		}
		event.Data = append(json.RawMessage(nil), data...)
		if err := validateLoadedEvent(event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load task %q events: %w", taskID, err)
	}
	return events, nil
}

func (store *SQLite) Commit(ctx context.Context, request erkaruntime.CommitRequest) (batch erkaruntime.CommittedBatch, err error) {
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	if err := erkaruntime.ValidateCommitRequest(request); err != nil {
		return batch, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return batch, fmt.Errorf("begin task %q commit: %w", request.TaskID, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	current, err := loadTaskTx(ctx, tx, request.TaskID)
	if err != nil {
		return batch, err
	}
	if current.LastItemSequence != request.ExpectedItemSequence || current.LastEventSequence != request.ExpectedEventSequence {
		return batch, fmt.Errorf("task %q sequences changed: %w", request.TaskID, erkaruntime.ErrConflict)
	}
	if request.Transition != nil && current.Status != request.Transition.From {
		return batch, fmt.Errorf("task %q status changed: %w", request.TaskID, erkaruntime.ErrConflict)
	}
	if err := validateTranscriptTx(ctx, tx, request); err != nil {
		return batch, err
	}
	if err := validateRunReferencesTx(ctx, tx, request); err != nil {
		return batch, err
	}
	if err := insertItemsTx(ctx, tx, request.Items); err != nil {
		return batch, err
	}
	if err := insertEventsTx(ctx, tx, request.Events); err != nil {
		return batch, err
	}

	lastItem := current.LastItemSequence
	if len(request.Items) > 0 {
		lastItem = request.Items[len(request.Items)-1].Sequence
	}
	lastEvent := current.LastEventSequence
	if len(request.Events) > 0 {
		lastEvent = request.Events[len(request.Events)-1].Sequence
	}
	status := current.Status
	if request.Transition != nil {
		status = request.Transition.To
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, item_sequence = ?, event_sequence = ?, updated_at = ?
		WHERE id = ? AND status = ? AND item_sequence = ? AND event_sequence = ?`,
		status, lastItem, lastEvent, formatTime(commitTime(request)),
		request.TaskID, current.Status, current.LastItemSequence, current.LastEventSequence,
	)
	if err != nil {
		return batch, fmt.Errorf("update task %q: %w", request.TaskID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return batch, fmt.Errorf("check task %q update: %w", request.TaskID, err)
	}
	if count != 1 {
		return batch, fmt.Errorf("task %q changed during commit: %w", request.TaskID, erkaruntime.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return batch, fmt.Errorf("commit task %q: %w", request.TaskID, err)
	}

	return erkaruntime.CommittedBatch{
		TaskID: request.TaskID, LastItemSequence: lastItem,
		LastEventSequence: lastEvent, Events: cloneEvents(request.Events),
	}, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanItem(row rowScanner, taskID erkaruntime.TaskID) (erkaruntime.RuntimeItem, error) {
	var item erkaruntime.RuntimeItem
	var kind, occurredAt string
	var payload []byte
	item.TaskID = taskID
	if err := row.Scan(&item.ID, &item.RunID, &item.Sequence, &kind, &occurredAt, &payload); err != nil {
		return erkaruntime.RuntimeItem{}, fmt.Errorf("scan task %q item: %w", taskID, err)
	}
	item.Kind = erkaruntime.ItemKind(kind)
	parsed, err := parseTime(occurredAt)
	if err != nil {
		return erkaruntime.RuntimeItem{}, fmt.Errorf("scan task %q item time: %w", taskID, err)
	}
	item.Time = parsed
	if err := unmarshalItemPayload(&item, payload); err != nil {
		return erkaruntime.RuntimeItem{}, fmt.Errorf("scan task %q item %q: %w", taskID, item.ID, err)
	}
	if err := erkaruntime.ValidateRuntimeItem(item); err != nil {
		return erkaruntime.RuntimeItem{}, fmt.Errorf("stored item %q is invalid: %w", item.ID, err)
	}
	return item, nil
}

func loadTaskTx(ctx context.Context, tx *sql.Tx, taskID erkaruntime.TaskID) (erkaruntime.TaskSnapshot, error) {
	var snapshot erkaruntime.TaskSnapshot
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT id, status, item_sequence, event_sequence FROM tasks WHERE id = ?`, taskID,
	).Scan(&snapshot.ID, &status, &snapshot.LastItemSequence, &snapshot.LastEventSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("task %q: %w", taskID, erkaruntime.ErrNotFound)
	}
	if err != nil {
		return erkaruntime.TaskSnapshot{}, fmt.Errorf("load task %q for commit: %w", taskID, err)
	}
	snapshot.Status = erkaruntime.TaskStatus(status)
	return snapshot, nil
}

func validateTranscriptTx(ctx context.Context, tx *sql.Tx, request erkaruntime.CommitRequest) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, sequence, kind, occurred_at, payload_json
		FROM runtime_items WHERE task_id = ? ORDER BY sequence`, request.TaskID)
	if err != nil {
		return fmt.Errorf("load task %q transcript for commit: %w", request.TaskID, err)
	}
	defer rows.Close()
	validator := erkaruntime.NewTranscriptValidator()
	for rows.Next() {
		item, scanErr := scanItem(rows, request.TaskID)
		if scanErr != nil {
			return scanErr
		}
		if err := validator.Apply(item); err != nil {
			return fmt.Errorf("stored transcript is invalid: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load task %q transcript for commit: %w", request.TaskID, err)
	}
	for _, item := range request.Items {
		if err := validator.Apply(item); err != nil {
			return err
		}
	}
	return nil
}

func validateRunReferencesTx(ctx context.Context, tx *sql.Tx, request erkaruntime.CommitRequest) error {
	seen := make(map[erkaruntime.RunID]struct{})
	check := func(runID erkaruntime.RunID) error {
		if runID == "" {
			return nil
		}
		if _, exists := seen[runID]; exists {
			return nil
		}
		var taskID erkaruntime.TaskID
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = ?`, runID).Scan(&taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %q: %w", runID, erkaruntime.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("load run %q: %w", runID, err)
		}
		if taskID != request.TaskID {
			return fmt.Errorf("run %q belongs to task %q", runID, taskID)
		}
		seen[runID] = struct{}{}
		return nil
	}
	for _, item := range request.Items {
		if err := check(item.RunID); err != nil {
			return err
		}
	}
	for _, event := range request.Events {
		if err := check(event.RunID); err != nil {
			return err
		}
	}
	if request.Transition != nil {
		return check(request.Transition.RunID)
	}
	return nil
}

func insertItemsTx(ctx context.Context, tx *sql.Tx, items []erkaruntime.RuntimeItem) error {
	for _, item := range items {
		payload, err := marshalItemPayload(item)
		if err != nil {
			return fmt.Errorf("encode runtime item %q: %w", item.ID, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO runtime_items(
				id, task_id, run_id, sequence, kind, occurred_at, payload_json
			) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			item.ID, item.TaskID, item.RunID, item.Sequence, item.Kind,
			formatTime(item.Time), payload,
		)
		if err != nil {
			if isConstraintError(err) {
				return fmt.Errorf("runtime item %q: %w", item.ID, erkaruntime.ErrAlreadyExists)
			}
			return fmt.Errorf("insert runtime item %q: %w", item.ID, err)
		}
	}
	return nil
}

func insertEventsTx(ctx context.Context, tx *sql.Tx, events []erkaruntime.Event) error {
	for _, event := range events {
		var runID any
		if event.RunID != "" {
			runID = event.RunID
		}
		var data any
		if len(event.Data) > 0 {
			data = []byte(event.Data)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO events(
				id, task_id, run_id, sequence, type, occurred_at, message, data_json
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			event.ID, event.TaskID, runID, event.Sequence, event.Type,
			formatTime(event.Time), event.Message, data,
		)
		if err != nil {
			if isConstraintError(err) {
				return fmt.Errorf("event %q: %w", event.ID, erkaruntime.ErrAlreadyExists)
			}
			return fmt.Errorf("insert event %q: %w", event.ID, err)
		}
	}
	return nil
}

func marshalItemPayload(item erkaruntime.RuntimeItem) ([]byte, error) {
	switch item.Kind {
	case erkaruntime.ItemUserMessage, erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
		return json.Marshal(item.Message)
	case erkaruntime.ItemToolCall:
		return json.Marshal(item.ToolCall)
	case erkaruntime.ItemToolResult:
		return json.Marshal(item.ToolResult)
	case erkaruntime.ItemUsage:
		return json.Marshal(item.Usage)
	case erkaruntime.ItemCheckpoint:
		return json.Marshal(item.Checkpoint)
	case erkaruntime.ItemError:
		return json.Marshal(item.Error)
	default:
		return nil, fmt.Errorf("unknown runtime item kind %q", item.Kind)
	}
}

func unmarshalItemPayload(item *erkaruntime.RuntimeItem, payload []byte) error {
	if item == nil {
		return errors.New("runtime item is nil")
	}
	switch item.Kind {
	case erkaruntime.ItemUserMessage, erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
		item.Message = &erkaruntime.Message{}
		return json.Unmarshal(payload, item.Message)
	case erkaruntime.ItemToolCall:
		item.ToolCall = &erkaruntime.ToolCall{}
		return json.Unmarshal(payload, item.ToolCall)
	case erkaruntime.ItemToolResult:
		item.ToolResult = &erkaruntime.ToolResult{}
		return json.Unmarshal(payload, item.ToolResult)
	case erkaruntime.ItemUsage:
		item.Usage = &erkaruntime.Usage{}
		return json.Unmarshal(payload, item.Usage)
	case erkaruntime.ItemCheckpoint:
		item.Checkpoint = &erkaruntime.Checkpoint{}
		return json.Unmarshal(payload, item.Checkpoint)
	case erkaruntime.ItemError:
		item.Error = &erkaruntime.RuntimeError{}
		return json.Unmarshal(payload, item.Error)
	default:
		return fmt.Errorf("unknown runtime item kind %q", item.Kind)
	}
}

func validateLoadedEvent(event erkaruntime.Event) error {
	if err := erkaruntime.ValidateEvents(event.Sequence-1, []erkaruntime.Event{event}); err != nil {
		return fmt.Errorf("stored event %q is invalid: %w", event.ID, err)
	}
	return nil
}

func commitTime(request erkaruntime.CommitRequest) time.Time {
	var latest time.Time
	for _, item := range request.Items {
		if item.Time.After(latest) {
			latest = item.Time
		}
	}
	for _, event := range request.Events {
		if event.Time.After(latest) {
			latest = event.Time
		}
	}
	if latest.IsZero() {
		return time.Now().UTC()
	}
	return latest
}

func mapInsertError(err error, kind, id string) error {
	if err == nil {
		return nil
	}
	if isConstraintError(err) {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "foreign key") {
			return fmt.Errorf("%s %q dependency: %w", kind, id, erkaruntime.ErrNotFound)
		}
		return fmt.Errorf("%s %q: %w", kind, id, erkaruntime.ErrAlreadyExists)
	}
	return fmt.Errorf("create %s %q: %w", kind, id, err)
}

func isConstraintError(err error) bool {
	var sqliteErr *modernsqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19
}

func formatTime(value time.Time) string { return value.UTC().Format(timestampLayout) }

func parseTime(value string) (time.Time, error) { return time.Parse(timestampLayout, value) }

var _ erkaruntime.Store = (*SQLite)(nil)
