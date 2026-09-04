package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

type staticCatalog struct {
	models []erkaruntime.ModelInfo
	err    error
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
