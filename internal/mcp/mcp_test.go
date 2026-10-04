package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

func TestMain(m *testing.M) {
	if os.Getenv("ERKA_MCP_FIXTURE_SERVER") == "1" {
		runFixtureServer()
		return
	}
	os.Exit(m.Run())
}

// runFixtureServer is a stdio MCP server used by the tests.
func runFixtureServer() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	write := func(value any) {
		data, _ := json.Marshal(value)
		fmt.Fprintf(os.Stdout, "%s\n", data)
	}
	fmt.Fprintln(os.Stdout, "this line is not JSON-RPC and must be ignored")
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor    string          `json:"cursor"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			// Ask the client something first; it must answer ping.
			write(map[string]any{"jsonrpc": "2.0", "id": "server-1", "method": "ping"})
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture"}}})
		case "tools/list":
			if request.Params.Cursor == "" {
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"tools": []map[string]any{{"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}, "nextCursor": "page-2"}})
			} else {
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"tools": []map[string]any{
					{"name": "env", "description": "Report environment"},
					{"name": "bad schema", "inputSchema": map[string]any{"type": "string"}},
				}}})
			}
		case "tools/call":
			switch request.Params.Name {
			case "echo":
				var arguments struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(request.Params.Arguments, &arguments)
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": "echo: " + arguments.Text}, {"type": "image", "data": "AAAA", "mimeType": "image/png"}}}})
			case "env":
				_, secret := os.LookupEnv("FIXTURE_SECRET")
				_, forwarded := os.LookupEnv("FIXTURE_FORWARDED")
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("secret=%t forwarded=%t", secret, forwarded)}}, "isError": false}})
			default:
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
			}
		}
	}
}

func fixtureConfig(t *testing.T) ServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Command: executable, Args: []string{"-test.run=^$"}, EnvFrom: []string{"ERKA_MCP_FIXTURE_SERVER", "FIXTURE_FORWARDED"}, Timeout: 10 * time.Second}
}

func fixtureEnvironment(name string) (string, bool) {
	switch name {
	case "ERKA_MCP_FIXTURE_SERVER":
		return "1", true
	case "FIXTURE_FORWARDED", "FIXTURE_SECRET":
		return "value", true
	case "PATH":
		return os.Getenv("PATH"), true
	}
	return "", false
}

func TestStdioServerToolsThroughManager(t *testing.T) {
	t.Parallel()
	manager := Start(context.Background(), map[string]ServerConfig{"fixture": fixtureConfig(t), "off": {Command: "nothing", Disabled: true}}, t.TempDir(), fixtureEnvironment)
	defer manager.Close()
	tools := manager.Tools()
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Definition().Name)
	}
	if strings.Join(names, ",") != "mcp__fixture__echo,mcp__fixture__env" {
		t.Fatalf("tools = %v, warnings = %v", names, manager.Warnings())
	}
	if warnings := strings.Join(manager.Warnings(), ";"); !strings.Contains(warnings, "bad schema") {
		t.Fatalf("warnings = %q", warnings)
	}
	echo := tools[0]
	if !echo.Definition().Mutating || !strings.HasPrefix(echo.Definition().Description, "[MCP server fixture]") {
		t.Fatalf("definition = %#v", echo.Definition())
	}
	result, err := echo.Execute(context.Background(), erkaruntime.ToolInvocation{CallID: "c1", Arguments: json.RawMessage(`{"text":"hi"}`), IdempotencyKey: "k"})
	if err != nil || result.IsError || result.Output != "echo: hi\n[image content omitted]" {
		t.Fatalf("echo = %#v, %v", result, err)
	}
	result, err = tools[1].Execute(context.Background(), erkaruntime.ToolInvocation{CallID: "c2", Arguments: json.RawMessage(`{}`), IdempotencyKey: "k"})
	if err != nil || result.Output != "secret=false forwarded=true" {
		t.Fatalf("env = %#v, %v", result, err)
	}
}

func TestReadOnlyServerToolsAreNotMutating(t *testing.T) {
	t.Parallel()
	config := fixtureConfig(t)
	config.ReadOnly = true
	manager := Start(context.Background(), map[string]ServerConfig{"docs": config}, t.TempDir(), fixtureEnvironment)
	defer manager.Close()
	for _, tool := range manager.Tools() {
		if tool.Definition().Mutating {
			t.Fatalf("%s is mutating despite read_only", tool.Definition().Name)
		}
	}
}

func TestFailingServerBecomesWarning(t *testing.T) {
	t.Parallel()
	manager := Start(context.Background(), map[string]ServerConfig{"broken": {Command: "erka-definitely-missing-mcp-server", Timeout: time.Second}}, t.TempDir(), fixtureEnvironment)
	defer manager.Close()
	if len(manager.Tools()) != 0 || len(manager.Warnings()) != 1 || !strings.Contains(manager.Warnings()[0], "start MCP server") {
		t.Fatalf("tools = %d warnings = %v", len(manager.Tools()), manager.Warnings())
	}
}

func TestStreamableHTTPTransport(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var deleted bool
	var sawHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token-123" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.Method == http.MethodDelete {
			mu.Lock()
			deleted = request.Header.Get("Mcp-Session-Id") == "session-9"
			mu.Unlock()
			return
		}
		body, _ := io.ReadAll(request.Body)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &message)
		mu.Lock()
		sawHeaders = append(sawHeaders, message.Method+"|"+request.Header.Get("Mcp-Session-Id")+"|"+request.Header.Get("MCP-Protocol-Version"))
		mu.Unlock()
		switch message.Method {
		case "initialize":
			response.Header().Set("Mcp-Session-Id", "session-9")
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`, message.ID)
		case "notifications/initialized":
			response.WriteHeader(http.StatusAccepted)
		case "tools/list":
			response.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(response, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(response, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[{\"name\":\"search\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", message.ID)
		case "tools/call":
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"found 3"}],"isError":true}}`, message.ID)
		}
	}))
	defer server.Close()
	environment := func(name string) (string, bool) {
		if name == "REMOTE_TOKEN" {
			return "token-123", true
		}
		return "", false
	}
	client, err := Connect(context.Background(), "remote", ServerConfig{URL: server.URL, BearerTokenEnv: "REMOTE_TOKEN", Timeout: 5 * time.Second}, "", environment)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("tools = %#v, %v", tools, err)
	}
	result, err := client.CallTool(context.Background(), "search", nil)
	if err != nil || !result.IsError || result.Text() != "found 3" {
		t.Fatalf("call = %#v, %v", result, err)
	}
	_ = client.Close()
	mu.Lock()
	defer mu.Unlock()
	if !deleted {
		t.Fatal("session was not closed with DELETE")
	}
	if sawHeaders[0] != "initialize||" || sawHeaders[2] != "tools/list|session-9|2025-03-26" {
		t.Fatalf("headers = %v", sawHeaders)
	}
	if _, err := Connect(context.Background(), "remote", ServerConfig{URL: server.URL, BearerTokenEnv: "MISSING_TOKEN"}, "", environment); err == nil || !strings.Contains(err.Error(), "MISSING_TOKEN") {
		t.Fatalf("missing token error = %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	valid := map[string]ServerConfig{
		"github": {URL: "https://api.example.com/mcp", BearerTokenEnv: "GITHUB_TOKEN"},
		"local":  {URL: "http://127.0.0.1:8080/mcp"},
		"fs":     {Command: "mcp-fs", Args: []string{"--root", "."}, EnvFrom: []string{"FS_KEY"}, ReadOnly: true},
	}
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	for name, server := range map[string]ServerConfig{
		"Bad Name":  {Command: "x"},
		"a__b":      {Command: "x"},
		"both":      {Command: "x", URL: "https://x"},
		"neither":   {},
		"plainhttp": {URL: "http://mcp.example.com"},
		"userinfo":  {URL: "https://u:p@mcp.example.com"},
		"badenv":    {Command: "x", EnvFrom: []string{"1BAD"}},
		"tokenstd":  {Command: "x", BearerTokenEnv: "T"},
		"argsurl":   {URL: "https://x.example", Args: []string{"a"}},
		"timeout":   {Command: "x", Timeout: MaxTimeout + time.Second},
	} {
		if err := Validate(map[string]ServerConfig{name: server}); err == nil {
			t.Errorf("Validate(%s) succeeded", name)
		}
	}
	if ToolName("github", "create issue!") != "mcp__github__create_issue_" {
		t.Fatalf("ToolName = %s", ToolName("github", "create issue!"))
	}
	if len(ToolName("s", strings.Repeat("x", 100))) != maxToolNameLength {
		t.Fatal("tool names must be bounded")
	}
}
