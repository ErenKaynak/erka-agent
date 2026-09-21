package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type Tier string

const (
	TierTrivial  Tier = "trivial"
	TierStandard Tier = "standard"
	TierComplex  Tier = "complex"
)

// TriageEvent is the durable event recording a turn's tier, so a resumed
// pipeline keeps the stages it started with.
const TriageEvent = "task.triaged"

// Triage is one classification decision.
type Triage struct {
	Tier   Tier
	Source string
	Reason string
	Usage  erkaruntime.Usage
}

func ParseTier(value string) (Tier, bool) {
	switch Tier(strings.ToLower(strings.TrimSpace(value))) {
	case TierTrivial:
		return TierTrivial, true
	case TierStandard:
		return TierStandard, true
	case TierComplex:
		return TierComplex, true
	default:
		return "", false
	}
}

var (
	trivialPattern = regexp.MustCompile(`(?i)\b(typo|spelling|rename|bump|version|comment|docstring|readme|wording|copy ?edit|format|lint|whitespace|indent|log message|changelog|license header)\b`)
	complexPattern = regexp.MustCompile(`(?i)\b(refactor|architect|architecture|redesign|design|migrate|migration|rewrite|security|authentication|authorization|oauth|concurrency|race|deadlock|performance|optimi[sz]e|schema|protocol|distributed|across (the )?(codebase|repo|project|files)|multiple (files|packages|services)|end[- ]to[- ]end|new (feature|service|subsystem|module))\b`)
)

// ClassifyHeuristic triages a request locally at no token cost. It errs
// toward standard: only short, clearly mechanical requests are trivial.
func ClassifyHeuristic(objective string) Triage {
	text := strings.TrimSpace(objective)
	words := len(strings.Fields(text))
	lines := strings.Count(text, "\n") + 1
	switch {
	case complexPattern.MatchString(text):
		return Triage{Tier: TierComplex, Source: "heuristic", Reason: "request mentions work that usually spans design or several files"}
	case words > 150 || lines > 15:
		return Triage{Tier: TierComplex, Source: "heuristic", Reason: "long, detailed request"}
	case words <= 30 && trivialPattern.MatchString(text):
		return Triage{Tier: TierTrivial, Source: "heuristic", Reason: "short mechanical change"}
	default:
		return Triage{Tier: TierStandard, Source: "heuristic", Reason: "default tier"}
	}
}

const triageInstructions = `Classify the software task by the effort a coding agent needs.
trivial: a mechanical edit in one place (typo, rename, version bump, comment, formatting).
standard: an ordinary bug fix or small feature confined to a few files.
complex: design work, cross-cutting changes, migrations, security or concurrency-sensitive work, or anything ambiguous.
Reply with exactly one word: trivial, standard, or complex.`

// ClassifyWithModel asks a cheap model for the tier and falls back to the
// heuristic on any failure, so triage never blocks a task.
func ClassifyWithModel(ctx context.Context, provider erkaruntime.ModelProvider, objective string) Triage {
	fallback := ClassifyHeuristic(objective)
	if provider == nil {
		return fallback
	}
	request := erkaruntime.ModelRequest{
		Instructions:    triageInstructions,
		Items:           []erkaruntime.RuntimeItem{{Kind: erkaruntime.ItemUserMessage, Message: &erkaruntime.Message{Content: boundObjective(objective)}}},
		MaxOutputTokens: 16,
	}
	text, usage, err := collectText(ctx, provider, request)
	if err != nil {
		fallback.Reason = fmt.Sprintf("%s (model triage failed: %v)", fallback.Reason, err)
		return fallback
	}
	fields := strings.Fields(strings.Trim(text, " \n\t.`'\""))
	if len(fields) == 0 {
		fallback.Usage = usage
		return fallback
	}
	tier, ok := ParseTier(strings.Trim(fields[0], ".,:;`'\""))
	if !ok {
		fallback.Usage = usage
		fallback.Reason += " (model reply was not a tier)"
		return fallback
	}
	return Triage{Tier: tier, Source: "model:" + provider.Name(), Reason: "classified by triage model", Usage: usage}
}

func boundObjective(objective string) string {
	const limit = 8 << 10
	if len(objective) <= limit {
		return objective
	}
	return strings.ToValidUTF8(objective[:limit], "")
}

func collectText(ctx context.Context, provider erkaruntime.ModelProvider, request erkaruntime.ModelRequest) (string, erkaruntime.Usage, error) {
	stream, err := provider.Stream(ctx, request)
	if err != nil {
		return "", erkaruntime.Usage{}, err
	}
	if stream == nil {
		return "", erkaruntime.Usage{}, errors.New("provider returned a nil stream")
	}
	defer stream.Close()
	var builder strings.Builder
	var usage erkaruntime.Usage
	for {
		output, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return "", usage, errors.New("stream ended before completion")
		}
		if err != nil {
			return "", usage, err
		}
		switch output.Kind {
		case erkaruntime.ModelTextDelta:
			if builder.Len() < 256 {
				builder.WriteString(output.Text)
			}
		case erkaruntime.ModelUsage:
			usage.InputTokens += output.Usage.InputTokens
			usage.OutputTokens += output.Usage.OutputTokens
		case erkaruntime.ModelFailed:
			return "", usage, errors.New(output.Error.Message)
		case erkaruntime.ModelCompleted:
			return builder.String(), usage, nil
		}
	}
}

// ApplyTier returns a copy of the resolved configuration whose active
// pipeline runs only the tier's stages, with the tier's candidate overrides.
// Fixed pipelines are returned unchanged.
func ApplyTier(resolved config.Resolved, tier Tier) (config.Resolved, error) {
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok || pipeline.Routing != config.RoutingTiered {
		return resolved, nil
	}
	configured, ok := pipeline.Tiers[string(tier)]
	if !ok {
		return config.Resolved{}, fmt.Errorf("pipeline %q has no %s tier", resolved.ActivePipeline, tier)
	}
	stages := make(map[string]config.StageRoute, len(configured.Stages))
	for _, stage := range configured.Stages {
		route, ok := pipeline.Stages[stage]
		if !ok {
			return config.Resolved{}, fmt.Errorf("tier %s uses unconfigured stage %q", tier, stage)
		}
		route.Candidates = append([]string(nil), route.Candidates...)
		if override := configured.Candidates[stage]; len(override) > 0 {
			route.Candidates = append([]string(nil), override...)
		}
		stages[stage] = route
	}
	pipeline.Stages = stages
	pipelines := make(map[string]config.Pipeline, len(resolved.Pipelines))
	for name, value := range resolved.Pipelines {
		pipelines[name] = value
	}
	pipelines[resolved.ActivePipeline] = pipeline
	resolved.Pipelines = pipelines
	return resolved, nil
}

// FirstStage is where a pipeline starts: planning when configured, otherwise
// coding.
func FirstStage(resolved config.Resolved) Stage {
	if _, ok := resolved.Pipelines[resolved.ActivePipeline].Stages[string(StagePlanning)]; ok {
		return StagePlanning
	}
	return StageCoding
}

// RecordedTier reads the most recent triage decision from task events.
func RecordedTier(events []erkaruntime.Event) (Tier, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != TriageEvent {
			continue
		}
		var data struct {
			Tier string `json:"tier"`
		}
		if json.Unmarshal(events[index].Data, &data) == nil {
			return ParseTier(data.Tier)
		}
	}
	return "", false
}

func triageEventData(triage Triage) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"tier": triage.Tier, "source": triage.Source, "reason": triage.Reason, "input_tokens": triage.Usage.InputTokens, "output_tokens": triage.Usage.OutputTokens})
	return data
}
