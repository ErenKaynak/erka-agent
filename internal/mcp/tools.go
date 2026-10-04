package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	// ToolPrefix starts every MCP tool name exposed to the model.
	ToolPrefix        = "mcp__"
	maxToolNameLength = 64
	maxDescription    = 1024
	toolOutputLimit   = int64(256 << 10)
)

var unsafeNameCharacters = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ToolName returns the namespaced name erka exposes for a server tool.
func ToolName(server, tool string) string {
	name := ToolPrefix + server + "__" + unsafeNameCharacters.ReplaceAllString(tool, "_")
	if len(name) > maxToolNameLength {
		name = name[:maxToolNameLength]
	}
	return name
}

// Manager owns the connections for one session.
type Manager struct {
	mu       sync.Mutex
	clients  []*Client
	tools    []erkaruntime.Tool
	warnings []string
}

// Start connects to every enabled server concurrently. A server that fails
// to start or list tools is reported as a warning and skipped; it never
// prevents the session from running.
func Start(ctx context.Context, servers map[string]ServerConfig, workingDirectory string, environment Environment) *Manager {
	manager := &Manager{}
	names := make([]string, 0, len(servers))
	for name, server := range servers {
		if !server.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type outcome struct {
		client *Client
		tools  []erkaruntime.Tool
		warns  []string
		err    error
	}
	results := make([]outcome, len(names))
	var wait sync.WaitGroup
	for index, name := range names {
		wait.Add(1)
		go func(index int, name string) {
			defer wait.Done()
			server := servers[name]
			client, err := Connect(ctx, name, server, workingDirectory, environment)
			if err != nil {
				results[index] = outcome{err: err}
				return
			}
			infos, err := client.ListTools(ctx)
			if err != nil {
				_ = client.Close()
				results[index] = outcome{err: fmt.Errorf("list tools from MCP server %q: %w", name, err)}
				return
			}
			tools, warns := adaptTools(client, server, infos)
			results[index] = outcome{client: client, tools: tools, warns: warns}
		}(index, name)
	}
	wait.Wait()
	seen := make(map[string]bool)
	for _, result := range results {
		if result.err != nil {
			manager.warnings = append(manager.warnings, result.err.Error())
			continue
		}
		manager.clients = append(manager.clients, result.client)
		manager.warnings = append(manager.warnings, result.warns...)
		for _, tool := range result.tools {
			name := tool.Definition().Name
			if seen[name] {
				manager.warnings = append(manager.warnings, fmt.Sprintf("MCP tool name %q is duplicated after normalization; keeping the first", name))
				continue
			}
			seen[name] = true
			manager.tools = append(manager.tools, tool)
		}
	}
	return manager
}

// Tools returns the adapted tools in a stable order.
func (manager *Manager) Tools() []erkaruntime.Tool {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]erkaruntime.Tool(nil), manager.tools...)
}

// Warnings describes servers or tools that could not be used.
func (manager *Manager) Warnings() []string {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.warnings...)
}

// Close shuts down every connection.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	clients := manager.clients
	manager.clients, manager.tools = nil, nil
	manager.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
	return nil
}

func adaptTools(client *Client, server ServerConfig, infos []ToolInfo) ([]erkaruntime.Tool, []string) {
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	var tools []erkaruntime.Tool
	var warnings []string
	for _, info := range infos {
		if strings.TrimSpace(info.Name) == "" {
			continue
		}
		schema, err := normalizeSchema(info.InputSchema)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("MCP tool %s/%s skipped: %v", client.Name(), info.Name, err))
			continue
		}
		description := strings.TrimSpace(info.Description)
		if description == "" {
			description = info.Name
		}
		description = truncate(fmt.Sprintf("[MCP server %s] %s", client.Name(), description), maxDescription)
		tools = append(tools, &tool{client: client, remote: info.Name, definition: erkaruntime.ToolDefinition{
			Name: ToolName(client.Name(), info.Name), Description: description, InputSchema: schema,
			Mutating: !server.ReadOnly, ParallelSafe: false, OutputLimitBytes: toolOutputLimit,
		}})
	}
	return tools, warnings
}

// normalizeSchema compacts the advertised schema and checks that erka's
// validator accepts it, so one bad server tool cannot break a stage.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return nil, fmt.Errorf("invalid input schema: %w", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("invalid input schema: %w", err)
	}
	if object, ok := document.(map[string]any); !ok || (object["type"] != nil && object["type"] != "object") {
		return nil, fmt.Errorf("input schema must describe an object")
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("tool.json", document); err != nil {
		return nil, fmt.Errorf("invalid input schema: %w", err)
	}
	if _, err := compiler.Compile("tool.json"); err != nil {
		return nil, fmt.Errorf("unsupported input schema: %w", err)
	}
	return buffer.Bytes(), nil
}

type tool struct {
	client     *Client
	remote     string
	definition erkaruntime.ToolDefinition
}

func (tool *tool) Definition() erkaruntime.ToolDefinition { return tool.definition }

func (tool *tool) Execute(ctx context.Context, invocation erkaruntime.ToolInvocation) (erkaruntime.ToolExecutionResult, error) {
	result, err := tool.client.CallTool(ctx, tool.remote, invocation.Arguments)
	if err != nil {
		if ctx.Err() != nil {
			return erkaruntime.ToolExecutionResult{}, ctx.Err()
		}
		return erkaruntime.ToolExecutionResult{Output: err.Error(), IsError: true}, nil
	}
	return erkaruntime.ToolExecutionResult{Output: result.Text(), IsError: result.IsError}, nil
}

var _ erkaruntime.Tool = (*tool)(nil)
