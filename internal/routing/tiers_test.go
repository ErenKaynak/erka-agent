package routing

import (
	"context"
	"errors"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestClassifyHeuristic(t *testing.T) {
	t.Parallel()
	for objective, want := range map[string]Tier{
		"fix typo in the README":                                  TierTrivial,
		"bump version to 1.4.2":                                   TierTrivial,
		"the login form rejects valid emails, please fix it":      TierStandard,
		"add a --json flag to the status command":                 TierStandard,
		"refactor the session store to support multiple backends": TierComplex,
		"migrate the database schema to add soft deletes":         TierComplex,
		"rename getUser to loadUser across the codebase":          TierComplex,
	} {
		if got := ClassifyHeuristic(objective).Tier; got != want {
			t.Errorf("ClassifyHeuristic(%q) = %s, want %s", objective, got, want)
		}
	}
}

func TestClassifyWithModelParsesReplyAndFallsBack(t *testing.T) {
	t.Parallel()
	reply := func(text string) erkaruntime.ModelProvider {
		return &fixedTextProvider{text: text}
	}
	if got := ClassifyWithModel(context.Background(), reply("Complex."), "fix typo"); got.Tier != TierComplex || got.Source != "model:fixed" {
		t.Fatalf("model reply triage = %#v", got)
	}
	if got := ClassifyWithModel(context.Background(), reply("I think it depends"), "fix typo in README"); got.Tier != TierTrivial || got.Source != "heuristic" {
		t.Fatalf("unparseable reply triage = %#v", got)
	}
	if got := ClassifyWithModel(context.Background(), &fixedTextProvider{err: errors.New("offline")}, "fix typo in README"); got.Tier != TierTrivial {
		t.Fatalf("failed provider triage = %#v", got)
	}
}

func TestApplyTierFiltersStagesAndOverridesCandidates(t *testing.T) {
	t.Parallel()
	resolved := config.Resolved{ActivePipeline: "p", Pipelines: map[string]config.Pipeline{"p": {
		Routing: config.RoutingTiered,
		Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"a/plan"}}, "coding": {Candidates: []string{"a/big"}}, "review": {Candidates: []string{"a/plan"}},
		},
		Tiers: map[string]config.Tier{"trivial": {Stages: []string{"coding"}, Candidates: map[string][]string{"coding": {"a/small"}}}},
	}}}
	tiered, err := ApplyTier(resolved, TierTrivial)
	if err != nil {
		t.Fatal(err)
	}
	stages := tiered.Pipelines["p"].Stages
	if len(stages) != 1 || stages["coding"].Candidates[0] != "a/small" || FirstStage(tiered) != StageCoding {
		t.Fatalf("tiered stages = %#v", stages)
	}
	if resolved.Pipelines["p"].Stages["coding"].Candidates[0] != "a/big" {
		t.Fatal("ApplyTier mutated the base configuration")
	}
}

type fixedTextProvider struct {
	text string
	err  error
}

func (*fixedTextProvider) Name() string { return "fixed" }
func (*fixedTextProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{}, nil
}
func (provider *fixedTextProvider) Stream(context.Context, erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	if provider.err != nil {
		return nil, provider.err
	}
	return &fixedTextStream{outputs: []erkaruntime.ModelOutput{{Kind: erkaruntime.ModelTextDelta, Text: provider.text}, {Kind: erkaruntime.ModelCompleted}}}, nil
}

type fixedTextStream struct{ outputs []erkaruntime.ModelOutput }

func (stream *fixedTextStream) Next(context.Context) (erkaruntime.ModelOutput, error) {
	output := stream.outputs[0]
	stream.outputs = stream.outputs[1:]
	return output, nil
}
func (*fixedTextStream) Close() error { return nil }
