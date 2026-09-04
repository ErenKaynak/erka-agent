package catalog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type staticCatalog struct {
	models []erkaruntime.ModelInfo
	err    error
}

type catalogFunc func(context.Context) ([]erkaruntime.ModelInfo, error)

func (function catalogFunc) ListModels(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
	return function(ctx)
}

func (catalog staticCatalog) ListModels(context.Context) ([]erkaruntime.ModelInfo, error) {
	return catalog.models, catalog.err
}

func TestLoadMergesLiveAndConfiguredModelsPerStage(t *testing.T) {
	resolved := catalogFixture()
	result := Load(context.Background(), resolved, func(_ context.Context, name string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		switch name {
		case "chatgpt":
			return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", Default: true}}}, nil
		case "coder":
			return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-pro"}}}, nil
		default:
			return nil, errors.New("unexpected connection")
		}
	})
	if err := result.Error(); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 6 || len(result.Issues) != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertCatalogEntry(t, result, erkaruntime.StagePlanning, "chatgpt", "default", "configured", true, true)
	assertCatalogEntry(t, result, erkaruntime.StageReview, "chatgpt", "gpt-5.6-sol", "live", false, true)
	assertCatalogEntry(t, result, erkaruntime.StageCoding, "coder", "deepseek-v4-flash", "configured+live", true, true)
	assertCatalogEntry(t, result, erkaruntime.StageCoding, "coder", "deepseek-v4-pro", "live", false, true)
}

func TestLoadRetainsConfiguredFallbackOnPartialDiscoveryFailure(t *testing.T) {
	resolved := catalogFixture()
	result := Load(context.Background(), resolved, func(_ context.Context, name string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		if name == "coder" {
			return staticCatalog{err: errors.New("offline")}, nil
		}
		return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "gpt-5.6-sol"}}}, nil
	})
	if err := result.Error(); err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || result.Issues[0].Connection != "coder" {
		t.Fatalf("issues = %#v", result.Issues)
	}
	assertCatalogEntry(t, result, erkaruntime.StageCoding, "coder", "deepseek-v4-flash", "configured", true, true)
}

func TestLoadDisablesUnpricedDiscoveredModelForCostBudget(t *testing.T) {
	resolved := catalogFixture()
	resolved.Pipelines["default"].Stages["coding"] = config.StageRoute{
		Candidates: []string{"coder/deepseek-v4-flash"},
		Budget:     config.Budget{MaxCostMicrounits: 100, Currency: "USD"},
	}
	connection := resolved.Connections["coder"]
	connection.Models = map[string]config.ModelConfig{
		"deepseek-v4-flash": {InputCostPerMillionMicrounits: 1, OutputCostPerMillionMicrounits: 2, Currency: "USD"},
	}
	resolved.Connections["coder"] = connection
	result := Load(context.Background(), resolved, func(_ context.Context, name string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		if name == "coder" {
			return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-pro"}}}, nil
		}
		return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "gpt-5.6-sol"}}}, nil
	})
	assertCatalogEntry(t, result, erkaruntime.StageCoding, "coder", "deepseek-v4-flash", "configured+live", true, true)
	assertCatalogEntry(t, result, erkaruntime.StageCoding, "coder", "deepseek-v4-pro", "live", false, false)
}

func TestLoadOrdersStagesAndModelsDeterministically(t *testing.T) {
	resolved := catalogFixture()
	result := Load(context.Background(), resolved, func(_ context.Context, name string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		if name == "chatgpt" {
			return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "z-model"}, {ID: "a-model", Default: true}}}, nil
		}
		return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "z-model"}, {ID: "a-model"}}}, nil
	})
	got := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		got = append(got, string(entry.Stage)+":"+entry.Connection+"/"+entry.Model.ID)
	}
	want := []string{
		"planning:chatgpt/default", "planning:chatgpt/a-model", "planning:chatgpt/z-model",
		"coding:coder/deepseek-v4-flash", "coding:coder/a-model", "coding:coder/z-model",
		"review:chatgpt/default", "review:chatgpt/a-model", "review:chatgpt/z-model",
	}
	if len(got) != len(want) {
		t.Fatalf("ordered entries = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ordered entries = %#v, want %#v", got, want)
		}
	}
}

func TestLoadDiscoversConnectionsConcurrently(t *testing.T) {
	resolved := catalogFixture()
	started := make(chan string, len(resolved.Connections))
	release := make(chan struct{})
	var once sync.Once
	result := load(context.Background(), resolved, func(_ context.Context, name string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		return catalogFunc(func(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
			started <- name
			if len(started) == len(resolved.Connections) {
				once.Do(func() { close(release) })
			}
			select {
			case <-release:
				return []erkaruntime.ModelInfo{{ID: name + "-model"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}), nil
	}, 250*time.Millisecond)
	if len(result.Issues) != 0 || len(started) != len(resolved.Connections) {
		t.Fatalf("concurrent discovery = started %d, result %#v", len(started), result)
	}
}

func TestLoadAppliesDiscoveryTimeout(t *testing.T) {
	resolved := catalogFixture()
	result := load(context.Background(), resolved, func(context.Context, string, config.Connection, string) (erkaruntime.ModelCatalog, error) {
		return catalogFunc(func(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}), nil
	}, 20*time.Millisecond)
	if len(result.Issues) != len(resolved.Connections) {
		t.Fatalf("timeout issues = %#v", result.Issues)
	}
	for _, issue := range result.Issues {
		if !errors.Is(issue.Err, context.DeadlineExceeded) {
			t.Fatalf("%s timeout = %v", issue.Connection, issue.Err)
		}
	}
}

func TestLoadUsesTenSecondProductionDeadline(t *testing.T) {
	resolved := catalogFixture()
	deadlines := make(chan time.Duration, len(resolved.Connections))
	result := Load(context.Background(), resolved, func(ctx context.Context, _ string, _ config.Connection, _ string) (erkaruntime.ModelCatalog, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("discovery context has no deadline")
		}
		deadlines <- time.Until(deadline)
		return staticCatalog{models: []erkaruntime.ModelInfo{{ID: "available"}}}, nil
	})
	if len(result.Issues) != 0 {
		t.Fatalf("discovery issues = %#v", result.Issues)
	}
	close(deadlines)
	for remaining := range deadlines {
		if remaining < 9*time.Second || remaining > DiscoveryTimeout {
			t.Fatalf("discovery deadline = %s, want approximately %s", remaining, DiscoveryTimeout)
		}
	}
}

func TestResultErrorsOnlyWhenNoModelCanBeListed(t *testing.T) {
	if err := (Result{}).Error(); err == nil {
		t.Fatal("empty catalog error = nil")
	}
	if err := (Result{Entries: []Entry{{Model: erkaruntime.ModelInfo{ID: "configured"}}}, Issues: []Issue{{Connection: "offline", Err: errors.New("unavailable")}}}).Error(); err != nil {
		t.Fatalf("partial catalog error = %v", err)
	}
}

func catalogFixture() config.Resolved {
	return config.Resolved{
		ActivePipeline: "default",
		Connections: map[string]config.Connection{
			"chatgpt": {Class: config.ConnectionSubscriptionBridge, Provider: "codex", Models: map[string]config.ModelConfig{"default": {}}},
			"coder":   {Class: config.ConnectionNative, Provider: "deepseek"},
		},
		Pipelines: map[string]config.Pipeline{"default": {Stages: map[string]config.StageRoute{
			"planning": {Candidates: []string{"chatgpt/default"}},
			"coding":   {Candidates: []string{"coder/deepseek-v4-flash"}},
			"review":   {Candidates: []string{"chatgpt/default"}},
		}}},
	}
}

func assertCatalogEntry(t *testing.T, result Result, stage erkaruntime.Stage, connection, model, source string, current, selectable bool) {
	t.Helper()
	for _, entry := range result.Entries {
		if entry.Stage == stage && entry.Connection == connection && entry.Model.ID == model {
			if entry.Source != source || entry.Current != current || entry.Selectable != selectable {
				t.Fatalf("entry = %#v", entry)
			}
			return
		}
	}
	t.Fatalf("missing %s %s/%s in %#v", stage, connection, model, result.Entries)
}
