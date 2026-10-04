package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var compactionTime = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func message(sequence uint64, kind ItemKind, content string) RuntimeItem {
	return RuntimeItem{ID: ItemID("item-" + string(rune('a'+sequence))), Sequence: sequence, TaskID: "task", RunID: "run", Kind: kind, Time: compactionTime, Message: &Message{Content: content}}
}

func call(sequence uint64, id string) RuntimeItem {
	return RuntimeItem{ID: ItemID("call-" + id), Sequence: sequence, TaskID: "task", RunID: "run", Kind: ItemToolCall, Time: compactionTime, ToolCall: &ToolCall{CallID: ToolCallID(id), Tool: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}}
}

func result(sequence uint64, id, output string) RuntimeItem {
	return RuntimeItem{ID: ItemID("result-" + id), Sequence: sequence, TaskID: "task", RunID: "run", Kind: ItemToolResult, Time: compactionTime, ToolResult: &ToolResult{CallID: ToolCallID(id), Output: output}}
}

func TestApplyCompactionReplacesCoveredItemsAndKeepsAnchors(t *testing.T) {
	t.Parallel()
	checkpoint := RuntimeItem{ID: "checkpoint", Sequence: 2, TaskID: "task", RunID: "plan-run", Kind: ItemCheckpoint, Time: compactionTime, Checkpoint: &Checkpoint{Objective: "x", ApprovedPlan: "plan"}}
	items := []RuntimeItem{
		message(1, ItemUserMessage, "fix it"), checkpoint,
		call(3, "a"), result(4, "a", "old output"), message(5, ItemAssistantMessage, "looked"),
		call(6, "b"), result(7, "b", "new output"),
	}
	compaction := RuntimeItem{ID: "compaction-1", Sequence: 8, TaskID: "task", RunID: "run", Kind: ItemCompaction, Time: compactionTime, Compaction: &CompactionRecord{ThroughSequence: 5, Summary: "read a.go"}}
	got := applyCompaction(items, append(append([]RuntimeItem(nil), items...), compaction), StageRequest{Stage: StageCoding, RunID: "run"})
	var kinds []string
	for _, item := range got {
		kinds = append(kinds, string(item.ID))
	}
	if strings.Join(kinds, ",") != "item-b,checkpoint,compaction:compaction-1,call-b,result-b" {
		t.Fatalf("items = %v", kinds)
	}
	if !strings.Contains(got[2].Message.Content, "read a.go") || got[2].Kind != ItemUserMessage {
		t.Fatalf("summary = %#v", got[2])
	}
	// A compaction from another run does not apply to this pipeline stage.
	other := compaction
	other.RunID = "other-run"
	if len(applyCompaction(items, []RuntimeItem{other}, StageRequest{Stage: StageCoding, RunID: "run"})) != len(items) {
		t.Fatal("foreign-run compaction was applied")
	}
}

func TestCompactionCutNeverSplitsToolExchanges(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 1000)
	items := []RuntimeItem{
		call(1, "a"), result(2, "a", big), message(3, ItemAssistantMessage, big),
		call(4, "b"), call(5, "c"), result(6, "b", big), result(7, "c", big),
		message(8, ItemAssistantMessage, "done"),
	}
	for target := int64(500); target <= 6000; target += 250 {
		cut := compactionCut(items, 0, target)
		if cut == 0 {
			continue
		}
		kept := map[ToolCallID]bool{}
		for _, item := range items[cut:] {
			if item.Kind == ItemToolCall {
				kept[item.ToolCall.CallID] = true
			}
			if item.Kind == ItemToolResult && !kept[item.ToolResult.CallID] {
				t.Fatalf("target %d cut %d orphans result %s", target, cut, item.ToolResult.CallID)
			}
		}
	}
	if cut := compactionCut(items, 0, 1<<20); cut == 0 || cut == len(items) {
		t.Fatalf("roomy target cut = %d", cut)
	}
	if compactionCut(items[:1], 0, 10) != 0 {
		t.Fatal("a lone tool call cannot be cut safely")
	}
}
