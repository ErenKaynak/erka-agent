package runtime

import (
	"context"
	"unicode/utf8"
)

const maxLiveTextBytes = 32 << 10

type LiveUpdateKind string

const (
	LiveTextDelta    LiveUpdateKind = "text_delta"
	LiveCommitted    LiveUpdateKind = "committed"
	LiveToolStarted  LiveUpdateKind = "tool_started"
	LiveToolFinished LiveUpdateKind = "tool_finished"
)

// LiveUpdate is an ephemeral, best-effort UI hint. Durable transcript data is
// always authoritative and observers must never use these updates as storage.
type LiveUpdate struct {
	TaskID  TaskID
	RunID   RunID
	Kind    LiveUpdateKind
	Text    string
	Tool    string
	CallID  ToolCallID
	IsError bool
}

// LiveObserver receives bounded updates. Implementations must return quickly;
// runtime correctness never depends on delivery.
type LiveObserver interface {
	Observe(context.Context, LiveUpdate)
}

type LiveObserverFunc func(context.Context, LiveUpdate)

func (function LiveObserverFunc) Observe(ctx context.Context, update LiveUpdate) {
	function(ctx, update)
}

func boundLiveText(value string) string {
	if len(value) <= maxLiveTextBytes {
		return value
	}
	data := []byte(value)[:maxLiveTextBytes]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}
