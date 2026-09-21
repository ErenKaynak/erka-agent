package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type ServerProject struct {
	ID             string
	Name           string
	RepositoryRoot string
	DefaultRef     string
	CreatedAt      time.Time
}

type ServerTaskStatus string

const (
	ServerTaskQueued    ServerTaskStatus = "queued"
	ServerTaskRunning   ServerTaskStatus = "running"
	ServerTaskWaiting   ServerTaskStatus = "waiting"
	ServerTaskCompleted ServerTaskStatus = "completed"
	ServerTaskFailed    ServerTaskStatus = "failed"
	ServerTaskCancelled ServerTaskStatus = "cancelled"
)

type ServerTask struct {
	TaskID          erkaruntime.TaskID
	ServerProjectID string
	WorktreeRoot    string
	Status          ServerTaskStatus
	ClaimHolder     string
	ClaimExpiresAt  time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Outcome records the accepted or rejected review of a completed task.
	// Once FinalizedAt is set the worktree is gone and the task is closed.
	Outcome      TaskOutcome
	ResultRef    string
	ResultBase   string
	ResultCommit string
	FinalizedAt  time.Time
}

type TaskOutcome string

const (
	TaskAccepted TaskOutcome = "accepted"
	TaskRejected TaskOutcome = "rejected"
)

const serverTaskColumns = `task_id, server_project_id, worktree_root, status, claim_holder, claim_expires_at, created_at, updated_at, outcome, result_ref, result_base, result_commit, finalized_at`

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
)

type Approval struct {
	ID          string
	TaskID      erkaruntime.TaskID
	Kind        string
	Message     string
	Status      ApprovalStatus
	Actor       string
	RequestedAt time.Time
	ExpiresAt   time.Time
	DecidedAt   time.Time
	// Tool, Details, and Options describe question and permission requests.
	Tool    string
	Details json.RawMessage
	Options []string
	// Response is the user's answer or note; Scope "task" extends an approved
	// permission to later calls of the same tool in the same task.
	Response string
	Scope    ApprovalScope
}

type ApprovalScope string

const (
	ApprovalScopeOnce ApprovalScope = "once"
	ApprovalScopeTask ApprovalScope = "task"
)

type ApprovalDecision struct {
	ID       string
	Actor    string
	Status   ApprovalStatus
	Response string
	Scope    ApprovalScope
	Time     time.Time
}

type APIToken struct {
	ID        string
	Name      string
	Hash      [32]byte
	Scopes    []string
	CreatedAt time.Time
	RevokedAt time.Time
}

type TokenPrincipal struct {
	ID     string
	Name   string
	Scopes []string
}

var validTokenScopes = map[string]bool{"read": true, "write": true, "approve": true, "admin": true}

func (store *SQLite) CreateServerProject(ctx context.Context, project ServerProject) error {
	if err := validateServerProject(project); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO server_projects(id, name, repository_root, default_ref, created_at) VALUES(?, ?, ?, ?, ?)`, project.ID, project.Name, project.RepositoryRoot, project.DefaultRef, formatTime(project.CreatedAt))
	return mapInsertError(err, "server project", project.ID)
}

func (store *SQLite) LoadServerProject(ctx context.Context, id string) (ServerProject, error) {
	return scanServerProject(store.db.QueryRowContext(ctx, `SELECT id, name, repository_root, default_ref, created_at FROM server_projects WHERE id = ?`, id), id)
}

func (store *SQLite) ListServerProjects(ctx context.Context) ([]ServerProject, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT id, name, repository_root, default_ref, created_at FROM server_projects ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ServerProject, 0)
	for rows.Next() {
		project, scanErr := scanServerProject(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, project)
	}
	return result, rows.Err()
}

func (store *SQLite) UpdateServerProject(ctx context.Context, id, name, defaultRef string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(defaultRef) == "" {
		return errors.New("server project update requires ID, name, and default ref")
	}
	result, err := store.db.ExecContext(ctx, `UPDATE server_projects SET name = ?, default_ref = ? WHERE id = ?`, strings.TrimSpace(name), strings.TrimSpace(defaultRef), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("server project %q: %w", id, erkaruntime.ErrNotFound)
	}
	return nil
}

func (store *SQLite) CreateServerTask(ctx context.Context, task ServerTask) error {
	if err := validateServerTask(task, false); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO server_tasks(task_id, server_project_id, worktree_root, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`, task.TaskID, task.ServerProjectID, task.WorktreeRoot, task.Status, formatTime(task.CreatedAt), formatTime(task.UpdatedAt))
	return mapInsertError(err, "server task", string(task.TaskID))
}

func (store *SQLite) LoadServerTask(ctx context.Context, taskID erkaruntime.TaskID) (ServerTask, error) {
	return scanServerTask(store.db.QueryRowContext(ctx, `SELECT `+serverTaskColumns+` FROM server_tasks WHERE task_id = ?`, taskID), taskID)
}

func (store *SQLite) ListServerTasks(ctx context.Context, projectID string) ([]ServerTask, error) {
	query := `SELECT ` + serverTaskColumns + ` FROM server_tasks`
	args := []any(nil)
	if projectID != "" {
		query += ` WHERE server_project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY updated_at DESC, task_id`
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ServerTask, 0)
	for rows.Next() {
		task, scanErr := scanServerTask(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func (store *SQLite) ClaimServerTask(ctx context.Context, holder string, now time.Time, duration time.Duration) (ServerTask, bool, error) {
	if strings.TrimSpace(holder) == "" || now.IsZero() || duration <= 0 {
		return ServerTask{}, false, errors.New("server task claim requires holder, time, and duration")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerTask{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `SELECT `+serverTaskColumns+` FROM server_tasks WHERE status = 'queued' OR (status = 'running' AND claim_expires_at <= ?) ORDER BY created_at, task_id LIMIT 1`, formatTime(now))
	task, err := scanServerTask(row, "")
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, erkaruntime.ErrNotFound) {
		return ServerTask{}, false, nil
	}
	if err != nil {
		return ServerTask{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE server_tasks SET status = 'running', claim_holder = ?, claim_expires_at = ?, updated_at = ? WHERE task_id = ? AND (status = 'queued' OR (status = 'running' AND claim_expires_at <= ?))`, holder, formatTime(now.Add(duration)), formatTime(now), task.TaskID, formatTime(now))
	if err != nil {
		return ServerTask{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ServerTask{}, false, erkaruntime.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ServerTask{}, false, err
	}
	task.Status, task.ClaimHolder, task.ClaimExpiresAt, task.UpdatedAt = ServerTaskRunning, holder, now.Add(duration).UTC(), now.UTC()
	return task, true, nil
}

func (store *SQLite) RenewServerTaskClaim(ctx context.Context, taskID erkaruntime.TaskID, holder string, now time.Time, duration time.Duration) (bool, error) {
	if taskID == "" || holder == "" || now.IsZero() || duration <= 0 {
		return false, errors.New("server task renewal requires task, holder, time, and duration")
	}
	result, err := store.db.ExecContext(ctx, `UPDATE server_tasks SET claim_expires_at = ?, updated_at = ? WHERE task_id = ? AND status = 'running' AND claim_holder = ? AND claim_expires_at > ?`, formatTime(now.Add(duration)), formatTime(now), taskID, holder, formatTime(now))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (store *SQLite) FinishServerTask(ctx context.Context, taskID erkaruntime.TaskID, holder string, status ServerTaskStatus, now time.Time) error {
	if status != ServerTaskWaiting && status != ServerTaskCompleted && status != ServerTaskFailed && status != ServerTaskCancelled {
		return fmt.Errorf("invalid terminal server task status %q", status)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE server_tasks SET status = ?, claim_holder = NULL, claim_expires_at = NULL, updated_at = ? WHERE task_id = ? AND status = 'running' AND claim_holder = ?`, status, formatTime(now), taskID, holder)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	switch status {
	case ServerTaskFailed:
		err = insertNotification(ctx, tx, taskNotification(taskID, "task.failed", "Task failed", "The task stopped with an error. Inspect its events, then retry or send a follow-up.", now))
	case ServerTaskWaiting:
		var pending int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM approvals WHERE task_id = ? AND status = 'pending'`, taskID).Scan(&pending); err == nil && pending == 0 {
			err = insertNotification(ctx, tx, taskNotification(taskID, "task.waiting", "Task needs attention", "An interrupted tool outcome needs inspection before the task can continue.", now))
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (store *SQLite) CompleteServerTaskWithApproval(ctx context.Context, taskID erkaruntime.TaskID, holder string, approval Approval) error {
	if approval.TaskID != taskID {
		return errors.New("completion approval identifies a different task")
	}
	if err := validateApproval(approval); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE server_tasks SET status = 'completed', claim_holder = NULL, claim_expires_at = NULL, updated_at = ? WHERE task_id = ? AND status = 'running' AND claim_holder = ?`, formatTime(approval.RequestedAt), taskID, holder)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	if err := insertApproval(ctx, tx, approval); err != nil {
		return err
	}
	if err := insertNotification(ctx, tx, approvalNotification(approval)); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *SQLite) ReleaseServerTaskClaim(ctx context.Context, taskID erkaruntime.TaskID, holder string, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE server_tasks SET status = 'queued', claim_holder = NULL, claim_expires_at = NULL, updated_at = ? WHERE task_id = ? AND status = 'running' AND claim_holder = ?`, formatTime(now), taskID, holder)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (store *SQLite) QueueServerTask(ctx context.Context, taskID erkaruntime.TaskID, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE server_tasks SET status = 'queued', claim_holder = NULL, claim_expires_at = NULL, updated_at = ? WHERE task_id = ? AND status IN ('waiting', 'completed', 'failed') AND finalized_at IS NULL`, formatTime(now), taskID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (store *SQLite) CancelServerTask(ctx context.Context, taskID erkaruntime.TaskID, now time.Time) (bool, error) {
	result, err := store.db.ExecContext(ctx, `UPDATE server_tasks SET status = 'cancelled', claim_holder = NULL, claim_expires_at = NULL, updated_at = ? WHERE task_id = ? AND status IN ('queued', 'waiting')`, formatTime(now), taskID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// CreateApproval records a pending approval and its notification atomically.
func (store *SQLite) CreateApproval(ctx context.Context, approval Approval) error {
	if err := validateApproval(approval); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertApproval(ctx, tx, approval); err != nil {
		return err
	}
	if err := insertNotification(ctx, tx, approvalNotification(approval)); err != nil {
		return err
	}
	return tx.Commit()
}

const approvalColumns = `id, task_id, kind, message, status, actor, requested_at, expires_at, decided_at, tool, details_json, options_json, response, scope`

func (store *SQLite) LoadApproval(ctx context.Context, id string) (Approval, error) {
	return scanApproval(store.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id), id)
}

// HasTaskGrant reports whether the user approved a tool for the rest of a task.
func (store *SQLite) HasTaskGrant(ctx context.Context, taskID erkaruntime.TaskID, tool string) (bool, error) {
	var count int
	err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM approvals WHERE task_id = ? AND kind = 'permission' AND tool = ? AND status = 'approved' AND scope = 'task'`, taskID, tool).Scan(&count)
	return count > 0, err
}

func (store *SQLite) ListApprovals(ctx context.Context, taskID erkaruntime.TaskID, status ApprovalStatus) ([]Approval, error) {
	query := `SELECT ` + approvalColumns + ` FROM approvals WHERE 1 = 1`
	args := []any(nil)
	if taskID != "" {
		query += ` AND task_id = ?`
		args = append(args, taskID)
	}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY requested_at, id`
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Approval, 0)
	for rows.Next() {
		approval, scanErr := scanApproval(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (store *SQLite) DecideApproval(ctx context.Context, id, actor string, decision ApprovalStatus, now time.Time) error {
	return store.RecordApprovalDecision(ctx, ApprovalDecision{ID: id, Actor: actor, Status: decision, Time: now})
}

// RecordApprovalDecision decides a pending, unexpired approval exactly once.
func (store *SQLite) RecordApprovalDecision(ctx context.Context, decision ApprovalDecision) error {
	if strings.TrimSpace(decision.Actor) == "" || decision.Time.IsZero() || (decision.Status != ApprovalApproved && decision.Status != ApprovalRejected) {
		return errors.New("approval decision requires actor, time, and approved or rejected status")
	}
	if decision.Scope == "" {
		decision.Scope = ApprovalScopeOnce
	}
	if decision.Scope != ApprovalScopeOnce && decision.Scope != ApprovalScopeTask {
		return fmt.Errorf("unsupported approval scope %q", decision.Scope)
	}
	if len(decision.Response) > maxApprovalResponseBytes {
		return fmt.Errorf("approval response exceeds %d bytes", maxApprovalResponseBytes)
	}
	var response any
	if strings.TrimSpace(decision.Response) != "" {
		response = strings.TrimSpace(decision.Response)
	}
	now := decision.Time
	result, err := store.db.ExecContext(ctx, `UPDATE approvals SET status = ?, actor = ?, decided_at = ?, response = ?, scope = ? WHERE id = ? AND status = 'pending' AND (expires_at IS NULL OR expires_at > ?) AND (? = 'once' OR kind = 'permission')`, decision.Status, decision.Actor, formatTime(now), response, decision.Scope, decision.ID, formatTime(now), decision.Scope)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (store *SQLite) ExpireApprovals(ctx context.Context, now time.Time) (int64, error) {
	result, err := store.db.ExecContext(ctx, `UPDATE approvals SET status = 'expired', decided_at = ? WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at <= ?`, formatTime(now), formatTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (store *SQLite) CreateAPIToken(ctx context.Context, token APIToken) error {
	if err := validateAPIToken(token); err != nil {
		return err
	}
	scopes, _ := json.Marshal(normalizeScopes(token.Scopes))
	_, err := store.db.ExecContext(ctx, `INSERT INTO server_api_tokens(id, name, token_hash, scopes_json, created_at) VALUES(?, ?, ?, ?, ?)`, token.ID, token.Name, token.Hash[:], scopes, formatTime(token.CreatedAt))
	return mapInsertError(err, "API token", token.ID)
}

func (store *SQLite) RevokeAPIToken(ctx context.Context, id string, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE server_api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, formatTime(now), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrConflict
	}
	return nil
}

func (store *SQLite) AuthenticateAPIToken(ctx context.Context, hash [32]byte, requiredScope string) (TokenPrincipal, error) {
	if !validTokenScopes[requiredScope] {
		return TokenPrincipal{}, fmt.Errorf("unsupported required token scope %q", requiredScope)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id, name, token_hash, scopes_json FROM server_api_tokens WHERE revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return TokenPrincipal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var storedHash, encoded []byte
		if err := rows.Scan(&id, &name, &storedHash, &encoded); err != nil {
			return TokenPrincipal{}, err
		}
		var scopes []string
		if json.Unmarshal(encoded, &scopes) != nil || len(storedHash) != len(hash) {
			continue
		}
		if subtle.ConstantTimeCompare(storedHash, hash[:]) == 1 && (containsScope(scopes, requiredScope) || containsScope(scopes, "admin")) {
			return TokenPrincipal{ID: id, Name: name, Scopes: scopes}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return TokenPrincipal{}, err
	}
	return TokenPrincipal{}, errors.New("invalid or insufficient API token")
}

func validateServerProject(project ServerProject) error {
	if project.ID == "" || strings.TrimSpace(project.Name) == "" || strings.TrimSpace(project.DefaultRef) == "" || !filepath.IsAbs(project.RepositoryRoot) || project.CreatedAt.IsZero() {
		return errors.New("server project requires ID, name, absolute repository root, default ref, and creation time")
	}
	return nil
}

func validateServerTask(task ServerTask, claimed bool) error {
	if task.TaskID == "" || task.ServerProjectID == "" || !filepath.IsAbs(task.WorktreeRoot) || task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() || task.UpdatedAt.Before(task.CreatedAt) {
		return errors.New("server task requires IDs, absolute worktree root, and valid timestamps")
	}
	if claimed {
		return nil
	}
	if task.Status == "" {
		task.Status = ServerTaskQueued
	}
	if task.Status != ServerTaskQueued || task.ClaimHolder != "" || !task.ClaimExpiresAt.IsZero() {
		return errors.New("new server task must be unclaimed and queued")
	}
	return nil
}

func validateApproval(approval Approval) error {
	if approval.ID == "" || approval.TaskID == "" || strings.TrimSpace(approval.Kind) == "" || strings.TrimSpace(approval.Message) == "" || approval.RequestedAt.IsZero() {
		return errors.New("approval requires IDs, kind, message, and request time")
	}
	if approval.Status == "" {
		approval.Status = ApprovalPending
	}
	if approval.Status != ApprovalPending || approval.Actor != "" || !approval.DecidedAt.IsZero() || (!approval.ExpiresAt.IsZero() && !approval.ExpiresAt.After(approval.RequestedAt)) {
		return errors.New("new approval must be pending, undecided, and have a future expiry")
	}
	return nil
}

func validateAPIToken(token APIToken) error {
	if token.ID == "" || strings.TrimSpace(token.Name) == "" || token.CreatedAt.IsZero() || token.Hash == ([32]byte{}) {
		return errors.New("API token requires ID, name, hash, and creation time")
	}
	if !token.RevokedAt.IsZero() || len(normalizeScopes(token.Scopes)) == 0 {
		return errors.New("new API token requires scopes and must not be revoked")
	}
	for _, scope := range token.Scopes {
		if !validTokenScopes[strings.TrimSpace(scope)] {
			return fmt.Errorf("unsupported API token scope %q", scope)
		}
	}
	return nil
}

func normalizeScopes(scopes []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" && !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result
}

func containsScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func scanServerProject(row rowScanner, id string) (ServerProject, error) {
	var project ServerProject
	var created string
	if err := row.Scan(&project.ID, &project.Name, &project.RepositoryRoot, &project.DefaultRef, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ServerProject{}, fmt.Errorf("server project %q: %w", id, erkaruntime.ErrNotFound)
		}
		return ServerProject{}, err
	}
	var err error
	project.CreatedAt, err = parseTime(created)
	return project, err
}

func scanServerTask(row rowScanner, id erkaruntime.TaskID) (ServerTask, error) {
	return scanServerTaskWith(row, id)
}

// scanServerTaskWith scans serverTaskColumns followed by extra destinations.
func scanServerTaskWith(row rowScanner, id erkaruntime.TaskID, extra ...any) (ServerTask, error) {
	var task ServerTask
	var status, created, updated string
	var holder, expiry, outcome, ref, base, commit, finalized sql.NullString
	destinations := append([]any{&task.TaskID, &task.ServerProjectID, &task.WorktreeRoot, &status, &holder, &expiry, &created, &updated, &outcome, &ref, &base, &commit, &finalized}, extra...)
	if err := row.Scan(destinations...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ServerTask{}, fmt.Errorf("server task %q: %w", id, erkaruntime.ErrNotFound)
		}
		return ServerTask{}, err
	}
	task.Status = ServerTaskStatus(status)
	if holder.Valid {
		task.ClaimHolder = holder.String
	}
	var err error
	if expiry.Valid {
		task.ClaimExpiresAt, err = parseTime(expiry.String)
	}
	if err == nil {
		task.CreatedAt, err = parseTime(created)
	}
	if err == nil {
		task.UpdatedAt, err = parseTime(updated)
	}
	task.Outcome, task.ResultRef, task.ResultBase, task.ResultCommit = TaskOutcome(outcome.String), ref.String, base.String, commit.String
	if err == nil && finalized.Valid {
		task.FinalizedAt, err = parseTime(finalized.String)
	}
	return task, err
}

func scanApproval(row rowScanner, id string) (Approval, error) {
	var approval Approval
	var status, requested, scope string
	var actor, expires, decided, tool, response sql.NullString
	var details, options []byte
	if err := row.Scan(&approval.ID, &approval.TaskID, &approval.Kind, &approval.Message, &status, &actor, &requested, &expires, &decided, &tool, &details, &options, &response, &scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Approval{}, fmt.Errorf("approval %q: %w", id, erkaruntime.ErrNotFound)
		}
		return Approval{}, err
	}
	approval.Status = ApprovalStatus(status)
	approval.Scope = ApprovalScope(scope)
	approval.Actor, approval.Tool, approval.Response = actor.String, tool.String, response.String
	if len(details) > 0 {
		approval.Details = append(json.RawMessage(nil), details...)
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &approval.Options); err != nil {
			return Approval{}, fmt.Errorf("decode approval %q options: %w", approval.ID, err)
		}
	}
	var err error
	approval.RequestedAt, err = parseTime(requested)
	if err == nil && expires.Valid {
		approval.ExpiresAt, err = parseTime(expires.String)
	}
	if err == nil && decided.Valid {
		approval.DecidedAt, err = parseTime(decided.String)
	}
	return approval, err
}
