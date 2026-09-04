package bridges

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const openCodeReadOnlyConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "share": "disabled",
  "permission": {
    "edit": "deny",
    "bash": "deny",
    "task": "deny",
    "external_directory": "deny",
    "webfetch": "deny",
    "websearch": "deny",
    "skill": "deny"
  },
  "agent": {
    "plan": {
      "permission": {
        "edit": "deny",
        "bash": "deny",
        "task": "deny",
        "external_directory": "deny",
        "webfetch": "deny",
        "websearch": "deny",
        "skill": "deny"
      }
    }
  }
}`

type OpenCodeOptions struct {
	Executable string
	Runner     Runner
}

type OpenCode struct {
	executable string
	runner     Runner
}

func NewOpenCode(options OpenCodeOptions) *OpenCode {
	executable := strings.TrimSpace(options.Executable)
	if executable == "" {
		executable = "opencode"
	}
	runner := options.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	return &OpenCode{executable: executable, runner: runner}
}

func (*OpenCode) Name() string { return "opencode" }

func (*OpenCode) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, StructuredOutput: true}, nil
}

func (bridge *OpenCode) ListModels(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
	availability, err := bridge.Availability(ctx)
	if err != nil {
		return nil, err
	}
	if !availability.Available {
		return nil, providerFailure("opencode_unavailable", availability.Detail, true)
	}
	if !availability.Authenticated {
		return nil, providerFailure("opencode_auth", availability.Detail, false)
	}
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return nil, fmt.Errorf("find OpenCode CLI: %w", err)
	}
	directory, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	process, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: []string{"models", "--pure"}, Directory: directory,
		Environment: openCodeEnvironment(), OutputLimitBytes: 4 << 20,
	})
	if err != nil {
		return nil, err
	}
	if process.Truncated {
		return nil, errors.New("OpenCode model catalog output exceeded 4 MiB")
	}
	if process.ExitCode != 0 {
		return nil, providerFailure("opencode_models_failed", process.Stderr, retryableProcessFailure(process.Stderr))
	}
	models := parseOpenCodeModels(process.Stdout)
	if len(models) == 0 {
		return nil, errors.New("OpenCode model catalog contained no valid models")
	}
	return models, nil
}

func parseOpenCodeModels(output string) []erkaruntime.ModelInfo {
	seen := make(map[string]bool)
	models := make([]erkaruntime.ModelInfo, 0)
	for _, line := range strings.Split(output, "\n") {
		id := strings.TrimSpace(line)
		model := erkaruntime.ModelInfo{ID: id}
		if model.Validate() != nil || !strings.Contains(id, "/") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, model)
	}
	sort.Slice(models, func(left, right int) bool { return models[left].ID < models[right].ID })
	return models
}

func (bridge *OpenCode) Availability(ctx context.Context) (erkaruntime.BridgeAvailability, error) {
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeAvailability{Detail: "OpenCode CLI is not installed"}, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	version, err := bridge.runner.Run(ctx, Invocation{Executable: path, Arguments: []string{"--version"}, Directory: directory, OutputLimitBytes: 8 << 10})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	result := erkaruntime.BridgeAvailability{Available: version.ExitCode == 0, Version: strings.TrimSpace(version.Stdout)}
	if !result.Available {
		result.Detail = strings.TrimSpace(version.Stderr)
		return result, nil
	}
	auth, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: []string{"auth", "list", "--pure"}, Directory: directory,
		Environment: openCodeEnvironment(), OutputLimitBytes: 32 << 10,
	})
	if err != nil {
		return erkaruntime.BridgeAvailability{}, err
	}
	result.Authenticated = auth.ExitCode == 0 && storedCredentialCount(auth.Stdout) > 0
	if result.Authenticated {
		result.Detail = "OpenCode reports configured provider credentials"
	} else {
		result.Detail = "OpenCode has no reported provider credentials"
	}
	return result, nil
}

func storedCredentialCount(output string) int {
	fields := strings.Fields(strings.ToLower(output))
	for index, field := range fields {
		if field != "credentials" || index == 0 {
			continue
		}
		if count, err := strconv.Atoi(strings.Trim(fields[index-1], "│└─-")); err == nil {
			return count
		}
	}
	return 0
}

func (bridge *OpenCode) Execute(ctx context.Context, request erkaruntime.BridgeRequest) (erkaruntime.BridgeResult, error) {
	availability, err := bridge.Availability(ctx)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if !availability.Available {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_unavailable", availability.Detail, true)
	}
	if !availability.Authenticated {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_auth", availability.Detail, false)
	}
	path, err := bridge.runner.LookPath(bridge.executable)
	if err != nil {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_unavailable", err.Error(), true)
	}
	arguments := []string{"run", "--format", "json", "--pure", "--agent", "plan", "--dir", request.WorkingDirectory}
	if request.Model != "" && request.Model != "default" {
		arguments = append(arguments, "--model", request.Model)
	}
	process, err := bridge.runner.Run(ctx, Invocation{
		Executable: path, Arguments: arguments, Directory: request.WorkingDirectory,
		Environment: openCodeEnvironment(), Input: request.Prompt,
		OutputLimitBytes: outputBudget(request.MaxOutputBytes),
	})
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	if process.Truncated {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_output_limit", "OpenCode bridge process output exceeded its limit", false)
	}
	if process.ExitCode != 0 {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_failed", process.Stderr, retryableProcessFailure(process.Stderr))
	}
	return parseOpenCodeJSONL(request.Stage, process.Stdout, request.MaxOutputBytes)
}

func openCodeEnvironment() map[string]string {
	environment := selectedEnvironment("XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME")
	environment["OPENCODE_CONFIG_CONTENT"] = openCodeReadOnlyConfig
	environment["OPENCODE_DISABLE_PROJECT_CONFIG"] = "true"
	environment["OPENCODE_DISABLE_CLAUDE_CODE"] = "true"
	return environment
}

type openCodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Tokens struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
			Cache      struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error json.RawMessage `json:"error"`
}

func parseOpenCodeJSONL(stage erkaruntime.Stage, stream string, maxOutputBytes int64) (erkaruntime.BridgeResult, error) {
	scanner := bufio.NewScanner(strings.NewReader(stream))
	max := int(maxOutputBytes + (64 << 10))
	if max < 128<<10 {
		max = 128 << 10
	}
	scanner.Buffer(make([]byte, 64<<10), max)
	var output string
	var usage erkaruntime.Usage
	completed := false
	line := 0
	for scanner.Scan() {
		line++
		data := strings.TrimSpace(scanner.Text())
		if data == "" {
			continue
		}
		var event openCodeEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return erkaruntime.BridgeResult{}, fmt.Errorf("%w: OpenCode JSONL line %d: %v", ErrMalformedStream, line, err)
		}
		switch event.Type {
		case "text":
			if event.Part.Type == "text" && strings.TrimSpace(event.Part.Text) != "" {
				output = event.Part.Text
			}
		case "step_finish":
			completed = true
			usage.InputTokens += event.Part.Tokens.Input
			usage.OutputTokens += event.Part.Tokens.Output
			usage.CacheReadTokens += event.Part.Tokens.CacheRead + event.Part.Tokens.Cache.Read
			usage.CacheWriteTokens += event.Part.Tokens.CacheWrite + event.Part.Tokens.Cache.Write
		case "error":
			return erkaruntime.BridgeResult{}, providerFailure("opencode_turn_failed", string(event.Error), retryableProcessFailure(string(event.Error)))
		}
	}
	if err := scanner.Err(); err != nil {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: read OpenCode JSONL: %v", ErrMalformedStream, err)
	}
	if !completed || strings.TrimSpace(output) == "" {
		return erkaruntime.BridgeResult{}, fmt.Errorf("%w: OpenCode stream ended without a finished step and final text", ErrMalformedStream)
	}
	if maxOutputBytes > 0 && int64(len(output)) > maxOutputBytes {
		return erkaruntime.BridgeResult{}, providerFailure("opencode_output_limit", "OpenCode final text exceeded its limit", false)
	}
	checkpoint, err := decodeCheckpoint(stage, output)
	if err != nil {
		return erkaruntime.BridgeResult{}, err
	}
	return erkaruntime.BridgeResult{Output: output, Checkpoint: checkpoint, Usage: usage}, nil
}

var _ erkaruntime.SubscriptionBridge = (*OpenCode)(nil)
var _ erkaruntime.ModelCatalog = (*OpenCode)(nil)
