package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	maxApprovalResponseBytes = 16 << 10
	maxDeliveryErrorBytes    = 1 << 10
)

// Notification is one durable, target-neutral outbox entry. Entries are
// written in the same transaction as the state change they announce, so
// delivery is at-least-once and never ahead of committed state.
type Notification struct {
	ID         string
	Kind       string
	TaskID     erkaruntime.TaskID
	ApprovalID string
	Title      string
	Message    string
	Options    []string
	Objective  string
	CreatedAt  time.Time
}

// NotificationDelivery pairs a notification with one target that has not yet
// accepted it.
type NotificationDelivery struct {
	Notification Notification
	Target       string
	Attempts     int
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func insertApproval(ctx context.Context, executor sqlExecutor, approval Approval) error {
	var expires, tool, options any
	if !approval.ExpiresAt.IsZero() {
		expires = formatTime(approval.ExpiresAt)
	}
	if approval.Tool != "" {
		tool = approval.Tool
	}
	details := []byte(nil)
	if len(approval.Details) > 0 {
		if !json.Valid(approval.Details) {
			return errors.New("approval details must be valid JSON")
		}
		details = approval.Details
	}
	if len(approval.Options) > 0 {
		encoded, err := json.Marshal(approval.Options)
		if err != nil {
			return err
		}
		options = encoded
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO approvals(id, task_id, kind, message, status, requested_at, expires_at, tool, details_json, options_json, scope) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'once')`, approval.ID, approval.TaskID, approval.Kind, approval.Message, approval.Status, formatTime(approval.RequestedAt), expires, tool, details, options)
	return mapInsertError(err, "approval", approval.ID)
}

func approvalNotification(approval Approval) Notification {
	title := "Approval requested"
	switch approval.Kind {
	case "question":
		title = "Question from erka"
	case "permission":
		title = "Permission requested"
	case "accept_changes":
		title = "Task completed"
	}
	return Notification{
		ID: "notification_" + approval.ID, Kind: approval.Kind, TaskID: approval.TaskID, ApprovalID: approval.ID,
		Title: title, Message: approval.Message, Options: approval.Options, CreatedAt: approval.RequestedAt,
	}
}

func taskNotification(taskID erkaruntime.TaskID, kind, title, message string, now time.Time) Notification {
	sum := sha256.Sum256([]byte(string(taskID) + "\x00" + kind + "\x00" + formatTime(now)))
	return Notification{ID: "notification_" + hex.EncodeToString(sum[:16]), Kind: kind, TaskID: taskID, Title: title, Message: message, CreatedAt: now}
}

func insertNotification(ctx context.Context, executor sqlExecutor, notification Notification) error {
	var approvalID, options any
	if notification.ApprovalID != "" {
		approvalID = notification.ApprovalID
	}
	if len(notification.Options) > 0 {
		encoded, err := json.Marshal(notification.Options)
		if err != nil {
			return err
		}
		options = encoded
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO notifications(id, kind, task_id, approval_id, title, message, options_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, notification.ID, notification.Kind, notification.TaskID, approvalID, notification.Title, notification.Message, options, formatTime(notification.CreatedAt))
	return err
}

// PendingNotifications lists undelivered notification/target pairs that are
// due, oldest first. Notifications older than the horizon are not retried.
func (store *SQLite) PendingNotifications(ctx context.Context, targets []string, now time.Time, horizon time.Duration, limit int) ([]NotificationDelivery, error) {
	if len(targets) == 0 || limit <= 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(targets)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT n.id, n.kind, n.task_id, n.approval_id, n.title, n.message, n.options_json, n.created_at, COALESCE(t.objective, ''), target.value, COALESCE(d.attempts, 0)
		FROM notifications n
		JOIN json_each(?) AS target
		LEFT JOIN notification_deliveries d ON d.notification_id = n.id AND d.target = target.value
		LEFT JOIN tasks t ON t.id = n.task_id
		WHERE d.delivered_at IS NULL AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= ?) AND n.created_at >= ?
		ORDER BY n.created_at, n.id, target.value LIMIT ?`, string(encoded), formatTime(now), formatTime(now.Add(-horizon)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]NotificationDelivery, 0)
	for rows.Next() {
		var delivery NotificationDelivery
		notification := &delivery.Notification
		var approvalID sql.NullString
		var options []byte
		var created string
		if err := rows.Scan(&notification.ID, &notification.Kind, &notification.TaskID, &approvalID, &notification.Title, &notification.Message, &options, &created, &notification.Objective, &delivery.Target, &delivery.Attempts); err != nil {
			return nil, err
		}
		notification.ApprovalID = approvalID.String
		if len(options) > 0 {
			if err := json.Unmarshal(options, &notification.Options); err != nil {
				return nil, fmt.Errorf("decode notification %q options: %w", notification.ID, err)
			}
		}
		if notification.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		result = append(result, delivery)
	}
	return result, rows.Err()
}

func (store *SQLite) MarkNotificationDelivered(ctx context.Context, notificationID, target string, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `INSERT INTO notification_deliveries(notification_id, target, attempts, next_attempt_at, delivered_at) VALUES(?, ?, 1, ?, ?)
		ON CONFLICT(notification_id, target) DO UPDATE SET attempts = attempts + 1, delivered_at = excluded.delivered_at, last_error = NULL`, notificationID, target, formatTime(now), formatTime(now))
	return err
}

func (store *SQLite) MarkNotificationFailed(ctx context.Context, notificationID, target string, next time.Time, cause string) error {
	cause = strings.ToValidUTF8(cause, "")
	if len(cause) > maxDeliveryErrorBytes {
		cause = cause[:maxDeliveryErrorBytes]
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO notification_deliveries(notification_id, target, attempts, next_attempt_at, last_error) VALUES(?, ?, 1, ?, ?)
		ON CONFLICT(notification_id, target) DO UPDATE SET attempts = attempts + 1, next_attempt_at = excluded.next_attempt_at, last_error = excluded.last_error`, notificationID, target, formatTime(next), strings.ToValidUTF8(cause, ""))
	return err
}
