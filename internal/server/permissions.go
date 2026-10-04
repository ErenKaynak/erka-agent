package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

// PermissionApprovalKind marks approvals created for "ask" permission rules.
const PermissionApprovalKind = "tool_permission"

// ApprovalStore is the subset of the control store used for tool approvals.
type ApprovalStore interface {
	CreateApproval(context.Context, store.Approval) error
	LoadApproval(context.Context, string) (store.Approval, error)
}

type PermissionApproverOptions struct {
	IDs   erkaruntime.IDGenerator
	Clock func() time.Time
	// Poll is how often the approval is checked. Default 1s.
	Poll time.Duration
	// Expiry bounds how long a run waits. Default 1h; expiry denies.
	Expiry time.Duration
}

// PermissionApprover turns "ask" verdicts into durable approval requests
// that remote clients decide through POST /v1/approvals/{id}/decision. The
// first decision wins; later ones are rejected by the store.
type PermissionApprover struct {
	store   ApprovalStore
	options PermissionApproverOptions
}

func NewPermissionApprover(approvals ApprovalStore, options PermissionApproverOptions) *PermissionApprover {
	if options.IDs == nil {
		options.IDs = erkaruntime.RandomIDGenerator{}
	}
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Poll <= 0 {
		options.Poll = time.Second
	}
	if options.Expiry <= 0 {
		options.Expiry = time.Hour
	}
	return &PermissionApprover{store: approvals, options: options}
}

func (approver *PermissionApprover) RequestPermission(ctx context.Context, request erkaruntime.PermissionRequest) (bool, error) {
	if approver == nil || approver.store == nil {
		return false, errors.New("server permission approver is not configured")
	}
	id, err := approver.options.IDs.NewID("approval")
	if err != nil {
		return false, err
	}
	now := approver.options.Clock().UTC()
	message := "Allow " + request.Summary()
	if reason := strings.TrimSpace(request.Reason); reason != "" {
		message += " (" + reason + ")"
	}
	approval := store.Approval{ID: id, TaskID: request.TaskID, Kind: PermissionApprovalKind, Message: message, Status: store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(approver.options.Expiry)}
	if err := approver.store.CreateApproval(ctx, approval); err != nil {
		return false, fmt.Errorf("create permission approval: %w", err)
	}
	ticker := time.NewTicker(approver.options.Poll)
	defer ticker.Stop()
	for {
		current, err := approver.store.LoadApproval(ctx, id)
		if err != nil {
			return false, fmt.Errorf("load permission approval: %w", err)
		}
		switch current.Status {
		case store.ApprovalApproved:
			return true, nil
		case store.ApprovalRejected, store.ApprovalExpired:
			return false, nil
		}
		if !approver.options.Clock().Before(current.ExpiresAt) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

var _ erkaruntime.PermissionApprover = (*PermissionApprover)(nil)
