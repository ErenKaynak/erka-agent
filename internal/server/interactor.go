package server

import (
	"context"
	"errors"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

// InteractionStore is the durable state behind server questions and
// permission requests.
type InteractionStore interface {
	CreateApproval(context.Context, store.Approval) error
	LoadApproval(context.Context, string) (store.Approval, error)
	HasTaskGrant(context.Context, erkaruntime.TaskID, string) (bool, error)
}

// Interactor turns runtime user requests into durable approvals. Each request
// key maps to exactly one approval, so a resumed or restarted worker observes
// the same record and the person's decision instead of asking again.
type Interactor struct {
	store  InteractionStore
	clock  func() time.Time
	expiry time.Duration
}

func NewInteractor(interactionStore InteractionStore, clock func() time.Time, expiry time.Duration) (*Interactor, error) {
	if interactionStore == nil {
		return nil, errors.New("server interactor requires a store")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if expiry <= 0 {
		expiry = 7 * 24 * time.Hour
	}
	return &Interactor{store: interactionStore, clock: clock, expiry: expiry}, nil
}

func ApprovalIDForRequest(key string) string { return "approval_" + key }

func (interactor *Interactor) Request(ctx context.Context, request erkaruntime.UserRequest) (erkaruntime.UserResponse, error) {
	if err := request.Validate(); err != nil {
		return erkaruntime.UserResponse{}, err
	}
	id := ApprovalIDForRequest(request.Key)
	approval, err := interactor.store.LoadApproval(ctx, id)
	if errors.Is(err, erkaruntime.ErrNotFound) {
		if request.Kind == erkaruntime.UserPermission {
			granted, grantErr := interactor.store.HasTaskGrant(ctx, request.TaskID, request.Tool)
			if grantErr != nil {
				return erkaruntime.UserResponse{}, grantErr
			}
			if granted {
				return erkaruntime.UserResponse{Approved: true}, nil
			}
		}
		now := interactor.clock().UTC()
		createErr := interactor.store.CreateApproval(ctx, store.Approval{
			ID: id, TaskID: request.TaskID, Kind: string(request.Kind), Message: request.Message,
			Status: store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(interactor.expiry),
			Tool: request.Tool, Details: request.Details, Options: request.Options,
		})
		if createErr != nil && !errors.Is(createErr, erkaruntime.ErrAlreadyExists) {
			return erkaruntime.UserResponse{}, createErr
		}
		return erkaruntime.UserResponse{}, erkaruntime.ErrAwaitingUser
	}
	if err != nil {
		return erkaruntime.UserResponse{}, err
	}
	switch approval.Status {
	case store.ApprovalApproved:
		return erkaruntime.UserResponse{Approved: true, Answer: approval.Response}, nil
	case store.ApprovalRejected:
		return erkaruntime.UserResponse{Approved: false, Answer: approval.Response}, nil
	case store.ApprovalExpired:
		return erkaruntime.UserResponse{Approved: false}, nil
	default:
		return erkaruntime.UserResponse{}, erkaruntime.ErrAwaitingUser
	}
}

var _ erkaruntime.Interactor = (*Interactor)(nil)
var _ InteractionStore = (*store.SQLite)(nil)
