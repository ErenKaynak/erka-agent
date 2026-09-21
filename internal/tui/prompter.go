package tui

import (
	"context"
	"sync"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// PromptRequest is one question or permission request waiting for the person
// at the terminal.
type PromptRequest struct {
	Request erkaruntime.UserRequest
	reply   chan erkaruntime.UserResponse
}

// Prompter is the local-mode Interactor: a tool call blocks until the person
// answers in the TUI or the turn is cancelled. Answers are remembered by
// request key so a repeated call after recovery does not ask again.
type Prompter struct {
	requests chan *PromptRequest
	mu       sync.Mutex
	answers  map[string]erkaruntime.UserResponse
	granted  map[string]bool
}

func NewPrompter() *Prompter {
	return &Prompter{requests: make(chan *PromptRequest), answers: make(map[string]erkaruntime.UserResponse), granted: make(map[string]bool)}
}

func (prompter *Prompter) Requests() <-chan *PromptRequest { return prompter.requests }

func (prompter *Prompter) Request(ctx context.Context, request erkaruntime.UserRequest) (erkaruntime.UserResponse, error) {
	if err := request.Validate(); err != nil {
		return erkaruntime.UserResponse{}, err
	}
	prompter.mu.Lock()
	if response, ok := prompter.answers[request.Key]; ok {
		prompter.mu.Unlock()
		return response, nil
	}
	if request.Kind == erkaruntime.UserPermission && prompter.granted[request.Tool] {
		prompter.mu.Unlock()
		return erkaruntime.UserResponse{Approved: true}, nil
	}
	prompter.mu.Unlock()
	pending := &PromptRequest{Request: request, reply: make(chan erkaruntime.UserResponse, 1)}
	select {
	case prompter.requests <- pending:
	case <-ctx.Done():
		return erkaruntime.UserResponse{}, ctx.Err()
	}
	select {
	case response := <-pending.reply:
		prompter.mu.Lock()
		prompter.answers[request.Key] = response
		prompter.mu.Unlock()
		return response, nil
	case <-ctx.Done():
		return erkaruntime.UserResponse{}, ctx.Err()
	}
}

// grantSession approves a tool for the rest of this erka process.
func (prompter *Prompter) grantSession(tool string) {
	prompter.mu.Lock()
	prompter.granted[tool] = true
	prompter.mu.Unlock()
}

func (request *PromptRequest) answer(response erkaruntime.UserResponse) {
	select {
	case request.reply <- response:
	default:
	}
}

var _ erkaruntime.Interactor = (*Prompter)(nil)
