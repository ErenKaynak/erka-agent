package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ItemCompaction records a durable summary that replaces older transcript
// items in model context.
const ItemCompaction ItemKind = "compaction"

const (
	// compactionTriggerPercent of the context budget starts a compaction.
	compactionTriggerPercent = 80
	// compactionTargetPercent is the size the kept context aims for.
	compactionTargetPercent   = 50
	compactionMaxOutputTokens = int64(4096)
	compactionToolOutputBytes = 4 << 10
	syntheticSummaryPrefix    = "compaction:"
)

const compactionInstructions = `You compact an erka coding-agent transcript so work can continue in a smaller context window. Write a factual summary that preserves: the user's goals and constraints, decisions made, files inspected or changed (with paths), commands run and their results, errors encountered, what remains to be done, and any identifiers needed to continue. Do not invent facts, do not include secrets or credentials, and do not address the user. Output only the summary.`

// CompactionRecord is the durable result of one compaction. Items with
// Sequence <= ThroughSequence in the compaction's scope are replaced by
// Summary when context is built; resume reuses the summary verbatim and
// never asks a model again.
type CompactionRecord struct {
	ThroughSequence uint64 `json:"through_sequence"`
	Summary         string `json:"summary"`
	SummarizedItems int    `json:"summarized_items"`
	BeforeBytes     int64  `json:"before_bytes"`
	AfterBytes      int64  `json:"after_bytes"`
}

func (record CompactionRecord) Validate() error {
	if record.ThroughSequence == 0 || strings.TrimSpace(record.Summary) == "" {
		return errors.New("compaction record requires a through sequence and summary")
	}
	return nil
}

func contextBytes(instructions string, items []RuntimeItem) int64 {
	size := int64(len(instructions))
	for _, item := range items {
		size += contextItemBytes(item)
	}
	return size
}

// latestCompaction finds the compaction that applies to this request: in a
// pipeline stage only compactions from the same run, otherwise any.
func latestCompaction(history []RuntimeItem, request StageRequest) *RuntimeItem {
	for index := len(history) - 1; index >= 0; index-- {
		item := history[index]
		if item.Kind != ItemCompaction || item.Compaction == nil {
			continue
		}
		if request.Stage != "" && item.RunID != request.RunID {
			continue
		}
		return &history[index]
	}
	return nil
}

// applyCompaction replaces covered items with one synthetic summary
// message. Checkpoints and the latest user request are always kept.
func applyCompaction(items, history []RuntimeItem, request StageRequest) []RuntimeItem {
	compaction := latestCompaction(history, request)
	if compaction == nil {
		return items
	}
	through := compaction.Compaction.ThroughSequence
	latestUser := -1
	for index, item := range items {
		if item.Kind == ItemUserMessage && !isSyntheticSummary(item) {
			latestUser = index
		}
	}
	summary := RuntimeItem{
		ID: ItemID(syntheticSummaryPrefix + string(compaction.ID)), Sequence: compaction.Sequence,
		TaskID: compaction.TaskID, RunID: compaction.RunID, Kind: ItemUserMessage, Time: compaction.Time,
		Message: &Message{Content: "Summary of earlier work in this task (older context was compacted):\n" + compaction.Compaction.Summary},
	}
	result := make([]RuntimeItem, 0, len(items)+1)
	inserted := false
	for index, item := range items {
		covered := item.Sequence <= through && !isSyntheticSummary(item)
		protected := item.Kind == ItemCheckpoint || index == latestUser
		if covered && !protected {
			if !inserted {
				result = append(result, summary)
				inserted = true
			}
			continue
		}
		if !inserted && item.Sequence > through {
			result = append(result, summary)
			inserted = true
		}
		result = append(result, item)
	}
	if !inserted {
		result = append(result, summary)
	}
	return result
}

func isSyntheticSummary(item RuntimeItem) bool {
	return strings.HasPrefix(string(item.ID), syntheticSummaryPrefix)
}

// compactionCut picks how many leading compactable items to summarize. It
// only cuts after a completed exchange, so no kept tool result loses its
// call, and keeps as large a tail as fits under the target size.
func compactionCut(items []RuntimeItem, fixedBytes, target int64) int {
	calls := make(map[ToolCallID]int)
	for index, item := range items {
		if item.Kind == ItemToolCall {
			calls[item.ToolCall.CallID] = index
		}
	}
	safe := func(cut int) bool {
		if cut <= 0 || cut >= len(items) {
			return false
		}
		switch items[cut-1].Kind {
		case ItemToolResult, ItemAssistantMessage, ItemReasoningSummary, ItemUserMessage:
		default:
			return false
		}
		for _, item := range items[cut:] {
			if item.Kind == ItemToolResult {
				if callIndex, ok := calls[item.ToolResult.CallID]; ok && callIndex < cut {
					return false
				}
			}
		}
		return true
	}
	tail := int64(0)
	best := 0
	for cut := len(items) - 1; cut > 0; cut-- {
		tail += contextItemBytes(items[cut])
		if fixedBytes+tail > target {
			break
		}
		if safe(cut) {
			best = cut
		}
	}
	if best == 0 {
		// Even the smallest safe tail is over target; summarize as much as
		// possible while keeping the newest complete exchange.
		for cut := len(items) - 1; cut > 0; cut-- {
			if safe(cut) {
				return cut
			}
		}
	}
	return best
}

// compact summarizes older context into a durable compaction item.
func (backend *preparedBackend) compact(ctx context.Context, appender *durableAppender, request StageRequest, instructions string, modelItems []RuntimeItem, budget int64) ([]RuntimeItem, error) {
	before := contextBytes(instructions, modelItems)
	latestUser := -1
	for index, item := range modelItems {
		if item.Kind == ItemUserMessage && !isSyntheticSummary(item) {
			latestUser = index
		}
	}
	var candidates []RuntimeItem
	var previousSummary string
	fixed := int64(len(instructions))
	for index, item := range modelItems {
		switch {
		case isSyntheticSummary(item):
			previousSummary = item.Message.Content
		case item.Kind == ItemCheckpoint || index == latestUser:
			fixed += contextItemBytes(item)
		default:
			candidates = append(candidates, item)
		}
	}
	// Leave room for the summary itself.
	reserve := compactionMaxOutputTokens * 4
	if budget/10 < reserve {
		reserve = budget / 10
	}
	fixed += reserve
	cut := compactionCut(candidates, fixed, budget*compactionTargetPercent/100)
	if cut == 0 {
		return nil, nil
	}
	summarized := candidates[:cut]
	summary, usage, err := backend.summarize(ctx, request, previousSummary, summarized)
	if err != nil {
		return nil, err
	}
	record := CompactionRecord{ThroughSequence: summarized[len(summarized)-1].Sequence, Summary: summary, SummarizedItems: len(summarized), BeforeBytes: before}
	kept := append([]RuntimeItem(nil), modelItems...)
	record.AfterBytes = contextBytes(instructions, applyCompaction(kept, []RuntimeItem{{ID: "pending", Kind: ItemCompaction, RunID: request.RunID, Compaction: &record}}, StageRequest{}))
	items := []RuntimeItem{}
	if usage != nil {
		items = append(items, RuntimeItem{Kind: ItemUsage, Usage: usage})
	}
	items = append(items, RuntimeItem{Kind: ItemCompaction, Compaction: &record})
	return appender.append(ctx, items, []pendingEvent{{
		Type:    "context.compacted",
		Message: fmt.Sprintf("Compacted %d earlier items (%d → %d bytes)", record.SummarizedItems, record.BeforeBytes, record.AfterBytes),
		Data: eventData(map[string]any{
			"through_sequence": record.ThroughSequence, "summarized_items": record.SummarizedItems,
			"before_bytes": record.BeforeBytes, "after_bytes": record.AfterBytes, "budget_bytes": budget,
		}),
	}})
}

// summarize asks the stage's own provider for a summary. The transcript is
// rendered as plain text so the request carries no tool definitions or
// tool-use blocks and works with every provider.
func (backend *preparedBackend) summarize(ctx context.Context, request StageRequest, previousSummary string, items []RuntimeItem) (string, *Usage, error) {
	var transcript strings.Builder
	if previousSummary != "" {
		transcript.WriteString(previousSummary + "\n\n")
	}
	for _, item := range items {
		switch item.Kind {
		case ItemUserMessage:
			transcript.WriteString("USER: " + item.Message.Content + "\n")
		case ItemAssistantMessage, ItemReasoningSummary:
			transcript.WriteString("ASSISTANT: " + item.Message.Content + "\n")
		case ItemToolCall:
			transcript.WriteString(fmt.Sprintf("TOOL CALL %s (%s): %s\n", item.ToolCall.Tool, item.ToolCall.CallID, truncateText(string(item.ToolCall.Arguments), compactionToolOutputBytes)))
		case ItemToolResult:
			status := "ok"
			if item.ToolResult.IsError {
				status = "error"
			}
			transcript.WriteString(fmt.Sprintf("TOOL RESULT %s [%s]: %s\n", item.ToolResult.CallID, status, truncateText(item.ToolResult.Output, compactionToolOutputBytes)))
		}
	}
	maxTokens := backend.Limits.MaxOutputTokens
	if maxTokens > compactionMaxOutputTokens {
		maxTokens = compactionMaxOutputTokens
	}
	now := backend.Clock().UTC()
	modelRequest := ModelRequest{
		Instructions: compactionInstructions, MaxOutputTokens: maxTokens,
		Items: []RuntimeItem{{ID: "compaction-request", Sequence: 1, TaskID: request.TaskID, RunID: request.RunID, Kind: ItemUserMessage, Time: now, Message: &Message{Content: "<transcript>\n" + transcript.String() + "</transcript>\nSummarize this transcript."}}},
	}
	stream, err := backend.Provider.Stream(ctx, modelRequest)
	if err != nil {
		return "", nil, err
	}
	if stream == nil {
		return "", nil, fmt.Errorf("%w: provider returned a nil stream", ErrMalformedStream)
	}
	defer stream.Close()
	var text strings.Builder
	var usage *Usage
	for {
		output, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return "", nil, fmt.Errorf("%w: compaction stream ended before completion", ErrMalformedStream)
		}
		if err != nil {
			return "", nil, err
		}
		switch output.Kind {
		case ModelTextDelta:
			text.WriteString(output.Text)
			if int64(text.Len()) > backend.Limits.MaxModelOutputBytes {
				return "", nil, fmt.Errorf("%w: compaction summary exceeded the output limit", ErrMalformedStream)
			}
		case ModelUsage:
			if output.Usage != nil {
				if usage == nil {
					usage = &Usage{}
				}
				addUsage(usage, *output.Usage)
			}
		case ModelFailed:
			if output.Error != nil {
				return "", nil, &ProviderError{Code: output.Error.Code, Message: output.Error.Message, Retryable: output.Error.Retryable}
			}
			return "", nil, errors.New("compaction model call failed")
		case ModelCompleted:
			summary := strings.TrimSpace(text.String())
			if summary == "" {
				return "", nil, fmt.Errorf("%w: compaction produced an empty summary", ErrMalformedStream)
			}
			return summary, usage, nil
		}
	}
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…[truncated]"
}

// contextBudget is the byte budget for one stage: the stage's input-token
// budget, else the runtime-wide budget, else the model's context window.
func (backend *preparedBackend) contextBudget(ctx context.Context, request StageRequest) int64 {
	if request.ContextMaxBytes > 0 {
		return request.ContextMaxBytes
	}
	if backend.Limits.ContextBudgetBytes > 0 {
		return backend.Limits.ContextBudgetBytes
	}
	if capabilities, err := backend.Provider.Capabilities(ctx); err == nil && capabilities.ContextTokens > 0 {
		return capabilities.ContextTokens * 4
	}
	return 0
}
