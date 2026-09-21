package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/routing"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func tieredTestPipeline() config.Resolved {
	resolved := testSessionPipeline()
	pipeline := resolved.Pipelines["default"]
	pipeline.Routing = config.RoutingTiered
	pipeline.Tiers = map[string]config.Tier{
		"trivial":  {Stages: []string{"coding"}, Candidates: map[string][]string{"coding": {"coder/deepseek-flash"}}},
		"standard": {Stages: []string{"coding"}},
		"complex":  {Stages: []string{"planning", "coding"}},
	}
	resolved.Pipelines["default"] = pipeline
	return resolved
}

func TestTieredPipelineSkipsPlanningAndUsesCheapModelForTrivialRequests(t *testing.T) {
	for _, test := range []struct {
		name          string
		objective     string
		wantTier      routing.Tier
		wantPlanner   bool
		wantCodeModel string
	}{
		{name: "trivial", objective: "fix the typo in README", wantTier: routing.TierTrivial, wantCodeModel: "deepseek-flash"},
		{name: "complex", objective: "refactor the storage layer across the codebase", wantTier: routing.TierComplex, wantPlanner: true, wantCodeModel: "deepseek-test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, state := t.TempDir(), t.TempDir()
			writeFixture(t, root)
			initRepository(t, root)
			planner := &scriptedProvider{name: "openai", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "Plan."}, {Kind: erkaruntime.ModelCompleted}}}}
			coder := &scriptedProvider{name: "deepseek", responses: [][]erkaruntime.ModelOutput{{{Kind: erkaruntime.ModelTextDelta, Text: "Done."}, {Kind: erkaruntime.ModelCompleted}}}}
			models := []string{}
			session, err := OpenSession(context.Background(), SessionOptions{
				WorkingDirectory: root, StateDir: state, Pipeline: tieredTestPipeline(), IDs: &sequenceIDs{}, Clock: fixedClock,
				ProviderFactory: func(_ context.Context, candidate routing.Candidate) (erkaruntime.ModelProvider, error) {
					models = append(models, candidate.Model)
					switch candidate.Provider {
					case "openai":
						return planner, nil
					case "deepseek":
						return coder, nil
					}
					return nil, errors.New("unexpected provider")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			taskID, err := session.Submit(context.Background(), "", test.objective)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := session.RunNext(context.Background())
			if err != nil || result.Status != erkaruntime.TaskCompleted {
				t.Fatalf("RunNext() = %#v, %v", result, err)
			}
			if got := len(planner.Requests()) > 0; got != test.wantPlanner {
				t.Fatalf("planner called = %v, want %v", got, test.wantPlanner)
			}
			if models[len(models)-1] != test.wantCodeModel {
				t.Fatalf("coding model = %q, want %q (opened %v)", models[len(models)-1], test.wantCodeModel, models)
			}
			conversation, err := session.OpenConversation(context.Background(), taskID)
			if err != nil {
				t.Fatal(err)
			}
			tier, ok := routing.RecordedTier(conversation.Events)
			if !ok || tier != test.wantTier {
				t.Fatalf("recorded tier = %q, %v; want %q", tier, ok, test.wantTier)
			}
			for _, event := range conversation.Events {
				if event.Type == routing.TriageEvent {
					var data map[string]any
					if json.Unmarshal(event.Data, &data) != nil || data["source"] != "heuristic" {
						t.Fatalf("triage event data = %s", event.Data)
					}
				}
			}
		})
	}
}
