package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type Stage string

const (
	StagePlanning   Stage = "planning"
	StageCoding     Stage = "coding"
	StageValidation Stage = "validation"
	StageRepair     Stage = "repair"
	StageReview     Stage = "review"
)

var ErrNoRoute = errors.New("no eligible model route")
var ErrBudgetExceeded = errors.New("stage budget exceeded")

type Candidate struct {
	Reference  string
	Connection string
	Model      string
	Provider   string
	BaseURL    string
	APIKeyEnv  string
	Metadata   config.ModelConfig
}

type Budget struct {
	MaxInputTokens    int64
	MaxOutputTokens   int64
	MaxIterations     int
	Timeout           time.Duration
	MaxCostMicrounits int64
	Currency          string
}

type Route struct {
	Stage                Stage
	Candidates           []Candidate
	RequiredCapabilities []string
	Budget               Budget
}

// ResolveFixed converts one configured stage into a deterministic ordered
// route. It performs no network or credential access.
func ResolveFixed(resolved config.Resolved, stage Stage, defaults erkaruntime.NativeLimits) (Route, error) {
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return Route{}, fmt.Errorf("pipeline %q: %w", resolved.ActivePipeline, ErrNoRoute)
	}
	configured, ok := pipeline.Stages[string(stage)]
	if !ok {
		return Route{}, fmt.Errorf("stage %q is not configured: %w", stage, ErrNoRoute)
	}
	route := Route{Stage: stage, RequiredCapabilities: append([]string(nil), configured.RequiredCapabilities...)}
	for _, reference := range configured.Candidates {
		connectionName, model, _ := strings.Cut(reference, "/")
		connection := resolved.Connections[connectionName]
		route.Candidates = append(route.Candidates, Candidate{
			Reference: reference, Connection: connectionName, Model: model,
			Provider: connection.Provider, BaseURL: connection.BaseURL,
			APIKeyEnv: connection.APIKeyEnv, Metadata: connection.Models[model],
		})
	}
	budget, err := resolveBudget(configured.Budget, defaults)
	if err != nil {
		return Route{}, fmt.Errorf("stage %q budget: %w", stage, err)
	}
	route.Budget = budget
	return route, nil
}

func resolveBudget(value config.Budget, defaults erkaruntime.NativeLimits) (Budget, error) {
	if defaults == (erkaruntime.NativeLimits{}) {
		defaults = erkaruntime.DefaultNativeLimits()
	}
	result := Budget{
		MaxOutputTokens:   defaults.MaxOutputTokens,
		MaxIterations:     defaults.MaxIterations,
		Timeout:           defaults.StageTimeout,
		MaxInputTokens:    value.MaxInputTokens,
		MaxCostMicrounits: value.MaxCostMicrounits,
		Currency:          value.Currency,
	}
	if value.MaxOutputTokens > 0 {
		result.MaxOutputTokens = value.MaxOutputTokens
	}
	if value.MaxIterations > 0 {
		result.MaxIterations = value.MaxIterations
	}
	if value.Timeout != "" {
		parsed, err := time.ParseDuration(value.Timeout)
		if err != nil {
			return Budget{}, err
		}
		result.Timeout = parsed
	}
	return result, nil
}

// Eligible reports whether the provider and configured model metadata satisfy
// every stage capability. Provider capabilities are authoritative; configured
// metadata may narrow them but never broaden them.
func Eligible(route Route, candidate Candidate, capabilities erkaruntime.ModelCapabilities) error {
	for _, required := range route.RequiredCapabilities {
		available := providerCapability(capabilities, required)
		if available && len(candidate.Metadata.Capabilities) > 0 {
			available = contains(candidate.Metadata.Capabilities, required)
		}
		if !available {
			return fmt.Errorf("%s lacks required capability %q: %w", candidate.Reference, required, ErrNoRoute)
		}
	}
	if route.Budget.MaxInputTokens > 0 && capabilities.ContextTokens > 0 && route.Budget.MaxInputTokens > capabilities.ContextTokens {
		return fmt.Errorf("%s context limit is below the stage input budget: %w", candidate.Reference, ErrNoRoute)
	}
	return nil
}

func providerCapability(value erkaruntime.ModelCapabilities, capability string) bool {
	switch capability {
	case "tools":
		return value.Tools
	case "parallel_tools":
		return value.ParallelToolCalls
	case "structured_output":
		return value.StructuredOutput
	case "vision":
		return value.Vision
	default:
		return false
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func StandardStages(pipeline config.Pipeline) []Stage {
	result := []Stage{StagePlanning, StageCoding}
	if _, ok := pipeline.Stages[string(StageValidation)]; ok {
		result = append(result, StageValidation)
	}
	if _, ok := pipeline.Stages[string(StageReview)]; ok {
		result = append(result, StageReview)
	}
	return result
}

// PriceUsage converts provider-neutral token counts to configured microunits,
// rounding fractional microunits upward so budget enforcement is conservative.
func PriceUsage(usage erkaruntime.Usage, model config.ModelConfig) (erkaruntime.Usage, error) {
	if model.InputCostPerMillionMicrounits == 0 && model.OutputCostPerMillionMicrounits == 0 {
		return usage, nil
	}
	input, err := priceTokens(usage.InputTokens, model.InputCostPerMillionMicrounits)
	if err != nil {
		return erkaruntime.Usage{}, err
	}
	output, err := priceTokens(usage.OutputTokens, model.OutputCostPerMillionMicrounits)
	if err != nil || input > math.MaxInt64-output {
		return erkaruntime.Usage{}, errors.New("usage price overflows int64")
	}
	usage.CostMicrounits = input + output
	usage.Currency = model.Currency
	return usage, nil
}

func priceTokens(tokens, perMillion int64) (int64, error) {
	if tokens < 0 || perMillion < 0 || (tokens != 0 && perMillion > math.MaxInt64/tokens) {
		return 0, errors.New("usage price overflows int64")
	}
	product := tokens * perMillion
	priced := product / 1_000_000
	if product%1_000_000 != 0 {
		priced++
	}
	return priced, nil
}

func CheckBudget(budget Budget, usage erkaruntime.Usage, iterations int, elapsed time.Duration) error {
	switch {
	case budget.MaxInputTokens > 0 && usage.InputTokens > budget.MaxInputTokens:
		return fmt.Errorf("input tokens %d exceed %d: %w", usage.InputTokens, budget.MaxInputTokens, ErrBudgetExceeded)
	case budget.MaxOutputTokens > 0 && usage.OutputTokens > budget.MaxOutputTokens:
		return fmt.Errorf("output tokens %d exceed %d: %w", usage.OutputTokens, budget.MaxOutputTokens, ErrBudgetExceeded)
	case budget.MaxIterations > 0 && iterations > budget.MaxIterations:
		return fmt.Errorf("iterations %d exceed %d: %w", iterations, budget.MaxIterations, ErrBudgetExceeded)
	case budget.Timeout > 0 && elapsed > budget.Timeout:
		return fmt.Errorf("duration %s exceeds %s: %w", elapsed, budget.Timeout, ErrBudgetExceeded)
	case budget.MaxCostMicrounits > 0 && budget.Currency != "" && budget.Currency != usage.Currency:
		return fmt.Errorf("usage currency %q does not match budget currency %q: %w", usage.Currency, budget.Currency, ErrBudgetExceeded)
	case budget.MaxCostMicrounits > 0 && usage.CostMicrounits > budget.MaxCostMicrounits:
		return fmt.Errorf("cost %d exceeds %d microunits: %w", usage.CostMicrounits, budget.MaxCostMicrounits, ErrBudgetExceeded)
	default:
		return nil
	}
}

type FailureKind string

const (
	FailureConnection  FailureKind = "connection"
	FailureTimeout     FailureKind = "timeout"
	FailureRateLimit   FailureKind = "rate_limit"
	FailureUnavailable FailureKind = "unavailable"
	FailureAuth        FailureKind = "auth"
	FailureInvalid     FailureKind = "invalid_request"
	FailureUnknown     FailureKind = "unknown"
)

type AttemptError struct {
	Kind          FailureKind
	AfterMutation bool
	Err           error
}

func (failure *AttemptError) Error() string {
	if failure == nil || failure.Err == nil {
		return "route attempt failed"
	}
	return failure.Err.Error()
}

func (failure *AttemptError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}

func CanFallback(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var failure *AttemptError
	if !errors.As(err, &failure) || failure.AfterMutation {
		return false
	}
	switch failure.Kind {
	case FailureConnection, FailureTimeout, FailureRateLimit, FailureUnavailable:
		return true
	default:
		return false
	}
}
