package routing

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestResolveFixedPreservesCandidateOrderAndDefaultsBudgets(t *testing.T) {
	t.Parallel()
	resolved := fixtureConfig()
	route, err := ResolveFixed(resolved, StagePlanning, erkaruntime.NativeLimits{MaxIterations: 9, MaxOutputTokens: 900, MaxModelOutputBytes: 1, MaxCumulativeToolOutputBytes: 1, StageTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Candidates) != 2 || route.Candidates[0].Reference != "planner/strong" || route.Candidates[1].Reference != "coder/cheap" {
		t.Fatalf("candidates = %#v", route.Candidates)
	}
	if route.Budget.MaxIterations != 3 || route.Budget.MaxOutputTokens != 900 || route.Budget.Timeout != 2*time.Minute {
		t.Fatalf("budget = %#v", route.Budget)
	}
}

func TestEligibleUsesProviderAndConfiguredCapabilities(t *testing.T) {
	t.Parallel()
	route := Route{RequiredCapabilities: []string{"tools", "structured_output"}}
	candidate := Candidate{Reference: "x/model", Metadata: config.ModelConfig{Capabilities: []string{"tools", "structured_output"}}}
	capabilities := erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}
	if err := Eligible(route, candidate, capabilities); err != nil {
		t.Fatal(err)
	}
	candidate.Metadata.Capabilities = []string{"tools"}
	if err := Eligible(route, candidate, capabilities); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("metadata narrowing error = %v", err)
	}
}

func TestPriceUsageAndBudget(t *testing.T) {
	t.Parallel()
	usage, err := PriceUsage(erkaruntime.Usage{InputTokens: 1_500_000, OutputTokens: 500_000}, config.ModelConfig{InputCostPerMillionMicrounits: 2, OutputCostPerMillionMicrounits: 6, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostMicrounits != 6 || usage.Currency != "USD" {
		t.Fatalf("priced usage = %#v", usage)
	}
	if err := CheckBudget(Budget{MaxCostMicrounits: 5, Currency: "USD"}, usage, 1, time.Second); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if _, err := PriceUsage(erkaruntime.Usage{InputTokens: math.MaxInt64}, config.ModelConfig{InputCostPerMillionMicrounits: 2, Currency: "USD"}); err == nil {
		t.Fatal("PriceUsage() overflow error = nil")
	}
}

func TestCanFallbackIsDeterministicAndMutationSafe(t *testing.T) {
	t.Parallel()
	if !CanFallback(&AttemptError{Kind: FailureRateLimit, Err: errors.New("busy")}) {
		t.Fatal("rate limit should permit fallback")
	}
	if CanFallback(&AttemptError{Kind: FailureRateLimit, AfterMutation: true, Err: errors.New("busy")}) {
		t.Fatal("post-mutation failure permitted fallback")
	}
	if !CanFallback(&erkaruntime.StageError{Cause: &erkaruntime.ProviderError{Code: "http_503", Message: "busy", Retryable: true}}) {
		t.Fatal("retryable provider failure before mutation should permit fallback")
	}
	if CanFallback(&erkaruntime.StageError{Cause: &erkaruntime.ProviderError{Code: "http_503", Message: "busy", Retryable: true}, AfterMutation: true}) {
		t.Fatal("retryable provider failure after a tool mutation permitted fallback")
	}
	if CanFallback(&AttemptError{Kind: FailureAuth, Err: errors.New("bad key")}) {
		t.Fatal("auth failure permitted fallback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if CanFallback(ctx.Err()) {
		t.Fatal("cancellation permitted fallback")
	}
}

func TestStandardStagesIncludesOnlyConfiguredOptionalStages(t *testing.T) {
	t.Parallel()
	pipeline := fixtureConfig().Pipelines["default"]
	got := StandardStages(pipeline)
	want := []Stage{StagePlanning, StageCoding, StageReview}
	if len(got) != len(want) {
		t.Fatalf("stages = %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("stages = %#v", got)
		}
	}
}

func fixtureConfig() config.Resolved {
	return config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"planner": {Provider: "openai", BaseURL: "https://openai.example", APIKeyEnv: "OPENAI_KEY", Models: map[string]config.ModelConfig{"strong": {Capabilities: []string{"tools", "structured_output"}}}},
			"coder":   {Provider: "deepseek", BaseURL: "https://deepseek.example", APIKeyEnv: "DEEPSEEK_KEY"},
		},
		Pipelines: map[string]config.Pipeline{"default": {Routing: "fixed", MaxRepairAttempts: 1, Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"planner/strong", "coder/cheap"}, RequiredCapabilities: []string{"structured_output"}, Budget: config.Budget{MaxIterations: 3, Timeout: "2m"}},
			"coding":   {Candidates: []string{"coder/cheap"}},
			"review":   {Candidates: []string{"planner/strong"}},
		}}},
	}
}
