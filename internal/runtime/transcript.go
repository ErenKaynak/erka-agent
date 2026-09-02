package runtime

import (
	"errors"
	"fmt"
)

// TranscriptValidator checks ordering and stable tool-call relationships while
// a store replays or appends canonical items.
type TranscriptValidator struct {
	lastSequence uint64
	itemIDs      map[ItemID]struct{}
	toolCalls    map[ToolCallID]bool
}

func NewTranscriptValidator() *TranscriptValidator {
	return &TranscriptValidator{
		itemIDs:   make(map[ItemID]struct{}),
		toolCalls: make(map[ToolCallID]bool),
	}
}

func (validator *TranscriptValidator) Apply(item RuntimeItem) error {
	if validator == nil {
		return errors.New("transcript validator is nil")
	}
	if err := item.Validate(); err != nil {
		return err
	}
	if item.Sequence != validator.lastSequence+1 {
		return fmt.Errorf(
			"runtime item sequence %d does not follow %d",
			item.Sequence,
			validator.lastSequence,
		)
	}
	if _, exists := validator.itemIDs[item.ID]; exists {
		return fmt.Errorf("duplicate runtime item ID %q", item.ID)
	}

	switch item.Kind {
	case ItemToolCall:
		callID := item.ToolCall.CallID
		if _, exists := validator.toolCalls[callID]; exists {
			return fmt.Errorf("duplicate tool call ID %q", callID)
		}
		validator.toolCalls[callID] = false
	case ItemToolResult:
		callID := item.ToolResult.CallID
		completed, exists := validator.toolCalls[callID]
		if !exists {
			return fmt.Errorf("tool result references unknown call ID %q", callID)
		}
		if completed {
			return fmt.Errorf("tool call %q already has a result", callID)
		}
		validator.toolCalls[callID] = true
	}

	validator.itemIDs[item.ID] = struct{}{}
	validator.lastSequence = item.Sequence
	return nil
}

func (validator *TranscriptValidator) LastSequence() uint64 {
	if validator == nil {
		return 0
	}
	return validator.lastSequence
}

// ValidateEvents checks that a committed event batch is contiguous and unique.
func ValidateEvents(previousSequence uint64, events []Event) error {
	seen := make(map[EventID]struct{}, len(events))
	for index, event := range events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", index, err)
		}
		want := previousSequence + uint64(index) + 1
		if event.Sequence != want {
			return fmt.Errorf("event sequence %d, want %d", event.Sequence, want)
		}
		if _, exists := seen[event.ID]; exists {
			return fmt.Errorf("duplicate event ID %q", event.ID)
		}
		seen[event.ID] = struct{}{}
	}
	return nil
}
