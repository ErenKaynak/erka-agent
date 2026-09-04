package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const DiscoveryTimeout = 10 * time.Second

type Factory func(context.Context, string, config.Connection, string) (erkaruntime.ModelCatalog, error)

type Entry struct {
	Stage       erkaruntime.Stage
	Connection  string
	Class       string
	Provider    string
	Model       erkaruntime.ModelInfo
	Source      string
	State       string
	Detail      string
	Selectable  bool
	Current     bool
	Configured  bool
	ModelConfig config.ModelConfig
}

type Issue struct {
	Connection string
	Err        error
}

type Result struct {
	Entries []Entry
	Issues  []Issue
}

type connectionResult struct {
	name   string
	models []erkaruntime.ModelInfo
	err    error
}

func Load(ctx context.Context, resolved config.Resolved, factory Factory) Result {
	connections := configuredConnections(resolved)
	results := make(chan connectionResult, len(connections))
	var wait sync.WaitGroup
	if factory != nil {
		for name, connection := range connections {
			name, connection := name, connection
			wait.Add(1)
			go func() {
				defer wait.Done()
				queryCtx, cancel := context.WithTimeout(ctx, DiscoveryTimeout)
				defer cancel()
				catalog, err := factory(queryCtx, name, connection, representativeModel(resolved, name, connection))
				if err != nil {
					results <- connectionResult{name: name, err: err}
					return
				}
				models, err := catalog.ListModels(queryCtx)
				results <- connectionResult{name: name, models: models, err: err}
			}()
		}
	}
	go func() {
		wait.Wait()
		close(results)
	}()

	live := make(map[string][]erkaruntime.ModelInfo)
	issues := make([]Issue, 0)
	for result := range results {
		if result.err != nil {
			issues = append(issues, Issue{Connection: result.name, Err: result.err})
			continue
		}
		live[result.name] = result.models
	}
	if factory == nil {
		for name := range connections {
			issues = append(issues, Issue{Connection: name, Err: errors.New("live discovery is unavailable")})
		}
	}

	entries := make([]Entry, 0)
	for name, connection := range connections {
		class := connection.Class
		if class == "" {
			class = config.ConnectionNative
		}
		configured := configuredModels(resolved, name, connection)
		merged := make(map[string]Entry)
		for id, metadata := range configured {
			merged[id] = Entry{Connection: name, Class: class, Provider: connection.Provider, Model: erkaruntime.ModelInfo{ID: id}, Source: "configured", State: "configured", Selectable: true, Configured: true, ModelConfig: metadata}
		}
		for _, model := range live[name] {
			if model.Validate() != nil {
				continue
			}
			entry, exists := merged[model.ID]
			entry.Connection, entry.Class, entry.Provider = name, class, connection.Provider
			entry.Model = mergeModelInfo(entry.Model, model)
			entry.Selectable = true
			entry.State = "available"
			if exists {
				entry.Source = "configured+live"
			} else {
				entry.Source = "live"
			}
			merged[model.ID] = entry
		}
		stages := connectionStages(resolved, name)
		if len(stages) == 0 {
			stages = []erkaruntime.Stage{""}
		}
		for _, stage := range stages {
			for _, entry := range merged {
				entry.Stage = stage
				if stage == "" {
					entry.Selectable = false
					entry.Detail = "connection is not routed by the active pipeline"
				}
				entry.Current = currentReference(resolved, stage) == name+"/"+entry.Model.ID
				if budgetRequiresConfiguredPricing(resolved, stage) && !hasPricing(entry.ModelConfig) {
					entry.Selectable = false
					entry.Detail = "configure model pricing before using it with this stage's cost budget"
				}
				entries = append(entries, entry)
			}
		}
	}
	sortEntries(entries)
	sort.Slice(issues, func(left, right int) bool { return issues[left].Connection < issues[right].Connection })
	return Result{Entries: entries, Issues: issues}
}

func configuredConnections(resolved config.Resolved) map[string]config.Connection {
	if len(resolved.Connections) > 0 {
		result := make(map[string]config.Connection, len(resolved.Connections))
		for name, connection := range resolved.Connections {
			result[name] = connection
		}
		return result
	}
	return map[string]config.Connection{"default": {Class: config.ConnectionNative, Provider: resolved.Provider, BaseURL: resolved.BaseURL, APIKeyEnv: resolved.APIKeyEnv, Models: map[string]config.ModelConfig{resolved.Model: {}}}}
}

func configuredModels(resolved config.Resolved, name string, connection config.Connection) map[string]config.ModelConfig {
	models := make(map[string]config.ModelConfig, len(connection.Models)+2)
	for id, metadata := range connection.Models {
		models[id] = metadata
	}
	if len(resolved.Connections) == 0 {
		if id := strings.TrimSpace(resolved.Model); id != "" {
			models[id] = config.ModelConfig{}
		}
		return models
	}
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return models
	}
	for _, route := range pipeline.Stages {
		for _, reference := range route.Candidates {
			connectionName, model, ok := strings.Cut(reference, "/")
			if ok && connectionName == name {
				if _, exists := models[model]; !exists {
					models[model] = config.ModelConfig{}
				}
			}
		}
	}
	return models
}

func representativeModel(resolved config.Resolved, name string, connection config.Connection) string {
	if len(resolved.Connections) == 0 {
		return resolved.Model
	}
	for _, stage := range orderedStages() {
		pipeline := resolved.Pipelines[resolved.ActivePipeline]
		for _, reference := range pipeline.Stages[string(stage)].Candidates {
			connectionName, model, ok := strings.Cut(reference, "/")
			if ok && connectionName == name {
				return model
			}
		}
	}
	keys := make([]string, 0, len(connection.Models))
	for model := range connection.Models {
		keys = append(keys, model)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return "default"
}

func connectionStages(resolved config.Resolved, connection string) []erkaruntime.Stage {
	if len(resolved.Connections) == 0 {
		return []erkaruntime.Stage{erkaruntime.StageCoding}
	}
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return nil
	}
	stages := make([]erkaruntime.Stage, 0)
	for _, stage := range orderedStages() {
		for _, reference := range pipeline.Stages[string(stage)].Candidates {
			name, _, _ := strings.Cut(reference, "/")
			if name == connection {
				stages = append(stages, stage)
				break
			}
		}
	}
	return stages
}

func currentReference(resolved config.Resolved, stage erkaruntime.Stage) string {
	if len(resolved.Connections) == 0 {
		return "default/" + resolved.Model
	}
	pipeline := resolved.Pipelines[resolved.ActivePipeline]
	if route, ok := pipeline.Stages[string(stage)]; ok && len(route.Candidates) > 0 {
		return route.Candidates[0]
	}
	return ""
}

func budgetRequiresConfiguredPricing(resolved config.Resolved, stage erkaruntime.Stage) bool {
	pipeline, ok := resolved.Pipelines[resolved.ActivePipeline]
	if !ok {
		return false
	}
	return pipeline.Stages[string(stage)].Budget.MaxCostMicrounits > 0
}

func hasPricing(model config.ModelConfig) bool {
	return strings.TrimSpace(model.Currency) != "" && (model.InputCostPerMillionMicrounits > 0 || model.OutputCostPerMillionMicrounits > 0)
}

func mergeModelInfo(configured, live erkaruntime.ModelInfo) erkaruntime.ModelInfo {
	result := configured
	if live.ID != "" {
		result.ID = live.ID
	}
	if live.DisplayName != "" {
		result.DisplayName = live.DisplayName
	}
	if live.Description != "" {
		result.Description = live.Description
	}
	result.Default = result.Default || live.Default
	return result
}

func orderedStages() []erkaruntime.Stage {
	return []erkaruntime.Stage{erkaruntime.StagePlanning, erkaruntime.StageCoding, erkaruntime.StageValidation, erkaruntime.StageRepair, erkaruntime.StageReview}
}

func sortEntries(entries []Entry) {
	order := make(map[erkaruntime.Stage]int)
	for index, stage := range orderedStages() {
		order[stage] = index
	}
	sort.SliceStable(entries, func(left, right int) bool {
		a, b := entries[left], entries[right]
		if order[a.Stage] != order[b.Stage] {
			return order[a.Stage] < order[b.Stage]
		}
		if a.Connection != b.Connection {
			return a.Connection < b.Connection
		}
		if a.Current != b.Current {
			return a.Current
		}
		if a.Model.Default != b.Model.Default {
			return a.Model.Default
		}
		return a.Model.ID < b.Model.ID
	})
}

func (result Result) Error() error {
	if len(result.Entries) > 0 {
		return nil
	}
	if len(result.Issues) == 0 {
		return errors.New("no configured or discovered models could be listed")
	}
	messages := make([]string, 0, len(result.Issues))
	for _, issue := range result.Issues {
		messages = append(messages, fmt.Sprintf("%s: %v", issue.Connection, issue.Err))
	}
	return errors.New(strings.Join(messages, "; "))
}
