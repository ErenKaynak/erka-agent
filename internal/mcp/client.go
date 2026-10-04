package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision erka requests.
const ProtocolVersion = "2025-06-18"

const (
	maxMessageBytes = 16 << 20
	maxErrorText    = 2 << 10
)

// ToolInfo is one tool advertised by a server.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallResult is the outcome of tools/call.
type CallResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MimeType string `json:"mimeType"`
		Resource *struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"resource"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// Text renders tool output as plain text for the model. Binary content is
// replaced by a short placeholder.
func (result CallResult) Text() string {
	var parts []string
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "resource":
			if block.Resource != nil && block.Resource.Text != "" {
				parts = append(parts, block.Resource.Text)
			} else if block.Resource != nil {
				parts = append(parts, "[resource "+block.Resource.URI+"]")
			}
		default:
			parts = append(parts, fmt.Sprintf("[%s content omitted]", block.Type))
		}
	}
	if len(parts) == 0 && len(result.StructuredContent) > 0 {
		parts = append(parts, string(result.StructuredContent))
	}
	return strings.Join(parts, "\n")
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (err *rpcError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", err.Code, truncate(err.Message, maxErrorText))
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type transport interface {
	call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// Client is one initialized MCP connection.
type Client struct {
	name      string
	config    ServerConfig
	transport transport
	nextID    atomic.Int64
	protocol  string
}

// Environment looks up environment variables; tests inject a fixed map.
type Environment func(string) (string, bool)

// Connect starts or dials the server and performs the initialize handshake.
func Connect(ctx context.Context, name string, config ServerConfig, workingDirectory string, environment Environment) (*Client, error) {
	if err := config.validate(name); err != nil {
		return nil, err
	}
	if environment == nil {
		environment = os.LookupEnv
	}
	var err error
	client := &Client{name: name, config: config}
	if config.Command != "" {
		client.transport, err = startStdio(config, workingDirectory, environment)
	} else {
		client.transport, err = newHTTPTransport(config, environment)
	}
	if err != nil {
		return nil, err
	}
	setupCtx, cancel := context.WithTimeout(ctx, config.timeout())
	defer cancel()
	result, err := client.call(setupCtx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "erka", "version": "1"},
	})
	if err != nil {
		_ = client.transport.close()
		return nil, fmt.Errorf("initialize MCP server %q: %w", name, err)
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		_ = client.transport.close()
		return nil, fmt.Errorf("initialize MCP server %q: %w", name, err)
	}
	client.protocol = initialized.ProtocolVersion
	if http, ok := client.transport.(*httpTransport); ok {
		http.setProtocol(initialized.ProtocolVersion)
	}
	if err := client.transport.notify(setupCtx, "notifications/initialized", nil); err != nil {
		_ = client.transport.close()
		return nil, fmt.Errorf("initialize MCP server %q: %w", name, err)
	}
	return client, nil
}

func (client *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return client.transport.call(ctx, client.nextID.Add(1), method, params)
}

// Name is the configured server name.
func (client *Client) Name() string { return client.name }

// ListTools returns every tool, following pagination cursors.
func (client *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, client.config.timeout())
	defer cancel()
	var tools []ToolInfo
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := client.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var listed struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &listed); err != nil {
			return nil, fmt.Errorf("decode tools/list: %w", err)
		}
		tools = append(tools, listed.Tools...)
		if listed.NextCursor == "" {
			return tools, nil
		}
		cursor = listed.NextCursor
	}
	return nil, errors.New("tools/list did not finish paginating")
}

// CallTool invokes one tool.
func (client *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, client.config.timeout())
	defer cancel()
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	result, err := client.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return CallResult{}, err
	}
	var called CallResult
	if err := json.Unmarshal(result, &called); err != nil {
		return CallResult{}, fmt.Errorf("decode tools/call: %w", err)
	}
	return called, nil
}

// Close shuts the connection down.
func (client *Client) Close() error { return client.transport.close() }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func decodeResponse(message rpcMessage) (json.RawMessage, error) {
	if message.Error != nil {
		return nil, message.Error
	}
	if len(message.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return message.Result, nil
}

// ---- stdio transport ----

// baseEnvironment is forwarded to every stdio server; everything else must
// be listed in env_from.
var baseEnvironment = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TEMP", "TMP", "USERPROFILE", "SYSTEMROOT", "COMSPEC", "PATHEXT", "WINDIR", "APPDATA", "LOCALAPPDATA"}

type stdioTransport struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpcMessage
	done    chan struct{}
	err     error
	stderr  *boundedBuffer
}

func startStdio(config ServerConfig, directory string, environment Environment) (*stdioTransport, error) {
	command := exec.Command(config.Command, config.Args...)
	command.Dir = directory
	command.Env = []string{}
	for _, name := range append(append([]string(nil), baseEnvironment...), config.EnvFrom...) {
		if value, ok := environment(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &boundedBuffer{limit: 8 << 10}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server: %w", err)
	}
	transport := &stdioTransport{command: command, stdin: stdin, pending: make(map[string]chan rpcMessage), done: make(chan struct{}), stderr: stderr}
	go transport.read(stdout)
	return transport, nil
}

func (transport *stdioTransport) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var message rpcMessage
		if json.Unmarshal(line, &message) != nil {
			continue
		}
		switch {
		case message.Method != "" && len(message.ID) > 0:
			transport.answerServerRequest(message)
		case message.Method == "" && len(message.ID) > 0:
			transport.mu.Lock()
			waiter := transport.pending[string(message.ID)]
			delete(transport.pending, string(message.ID))
			transport.mu.Unlock()
			if waiter != nil {
				waiter <- message
			}
		}
	}
	transport.mu.Lock()
	transport.err = errClosed
	if detail := strings.TrimSpace(transport.stderr.String()); detail != "" {
		transport.err = fmt.Errorf("%w: %s", errClosed, truncate(detail, maxErrorText))
	}
	transport.mu.Unlock()
	close(transport.done)
}

// answerServerRequest replies to server-initiated requests: ping succeeds,
// everything else (sampling, elicitation, roots) is unsupported.
func (transport *stdioTransport) answerServerRequest(message rpcMessage) {
	reply := rpcMessage{JSONRPC: "2.0", ID: message.ID}
	if message.Method == "ping" {
		reply.Result = json.RawMessage(`{}`)
	} else {
		reply.Error = &rpcError{Code: -32601, Message: "method not supported by erka"}
	}
	_ = transport.write(reply)
}

func (transport *stdioTransport) write(message rpcMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	_, err = transport.stdin.Write(append(data, '\n'))
	return err
}

func (transport *stdioTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	key := strconv.FormatInt(id, 10)
	waiter := make(chan rpcMessage, 1)
	transport.mu.Lock()
	if transport.err != nil {
		err := transport.err
		transport.mu.Unlock()
		return nil, err
	}
	transport.pending[key] = waiter
	transport.mu.Unlock()
	if err := transport.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params}); err != nil {
		transport.forget(key)
		return nil, fmt.Errorf("write MCP request: %w", err)
	}
	select {
	case message := <-waiter:
		return decodeResponse(message)
	case <-transport.done:
		transport.forget(key)
		transport.mu.Lock()
		defer transport.mu.Unlock()
		return nil, transport.err
	case <-ctx.Done():
		transport.forget(key)
		_ = transport.write(rpcMessage{JSONRPC: "2.0", Method: "notifications/cancelled", Params: map[string]any{"requestId": id, "reason": "cancelled by erka"}})
		return nil, ctx.Err()
	}
}

func (transport *stdioTransport) forget(key string) {
	transport.mu.Lock()
	delete(transport.pending, key)
	transport.mu.Unlock()
}

func (transport *stdioTransport) notify(_ context.Context, method string, params any) error {
	return transport.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
}

func (transport *stdioTransport) close() error {
	_ = transport.stdin.Close()
	exited := make(chan struct{})
	go func() { _ = transport.command.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = transport.command.Process.Kill()
		<-exited
	}
	return nil
}

type boundedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if room := buffer.limit - len(buffer.data); room > 0 {
		if len(value) > room {
			buffer.data = append(buffer.data, value[:room]...)
		} else {
			buffer.data = append(buffer.data, value...)
		}
	}
	return len(value), nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data)
}

// ---- streamable HTTP transport ----

type httpTransport struct {
	endpoint string
	token    string
	client   *http.Client
	mu       sync.Mutex
	session  string
	protocol string
}

func newHTTPTransport(config ServerConfig, environment Environment) (*httpTransport, error) {
	transport := &httpTransport{endpoint: config.URL, client: &http.Client{}}
	if config.BearerTokenEnv != "" {
		token, ok := environment(config.BearerTokenEnv)
		if !ok || strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("MCP bearer token is missing: set %s", config.BearerTokenEnv)
		}
		transport.token = token
	}
	return transport, nil
}

func (transport *httpTransport) setProtocol(version string) {
	transport.mu.Lock()
	transport.protocol = version
	transport.mu.Unlock()
}

func (transport *httpTransport) post(ctx context.Context, message rpcMessage) (*http.Response, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, transport.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	transport.mu.Lock()
	if transport.session != "" {
		request.Header.Set("Mcp-Session-Id", transport.session)
	}
	if transport.protocol != "" {
		request.Header.Set("MCP-Protocol-Version", transport.protocol)
	}
	transport.mu.Unlock()
	if transport.token != "" {
		request.Header.Set("Authorization", "Bearer "+transport.token)
	}
	response, err := transport.client.Do(request)
	if err != nil {
		// The URL error never contains the bearer token, which travels only
		// in a header.
		return nil, fmt.Errorf("MCP HTTP request failed: %w", err)
	}
	if session := response.Header.Get("Mcp-Session-Id"); session != "" {
		transport.mu.Lock()
		transport.session = session
		transport.mu.Unlock()
	}
	return response, nil
}

func (transport *httpTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	key := json.RawMessage(strconv.FormatInt(id, 10))
	response, err := transport.post(ctx, rpcMessage{JSONRPC: "2.0", ID: key, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorText))
		return nil, fmt.Errorf("MCP server returned HTTP %d: %s", response.StatusCode, truncate(strings.TrimSpace(string(body)), maxErrorText))
	}
	body := io.LimitReader(response.Body, maxMessageBytes)
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return readSSEResponse(body, string(key))
	}
	var message rpcMessage
	if err := json.NewDecoder(body).Decode(&message); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}
	return decodeResponse(message)
}

func readSSEResponse(body io.Reader, id string) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	var data []string
	flush := func() (json.RawMessage, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var message rpcMessage
		if json.Unmarshal([]byte(payload), &message) != nil || message.Method != "" || string(message.ID) != id {
			return nil, false, nil
		}
		result, err := decodeResponse(message)
		return result, true, err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if result, done, err := flush(); done {
				return result, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if result, done, err := flush(); done {
		return result, err
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read MCP event stream: %w", err)
	}
	return nil, errors.New("MCP event stream ended without a response")
}

func (transport *httpTransport) notify(ctx context.Context, method string, params any) error {
	response, err := transport.post(ctx, rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("MCP server returned HTTP %d for %s", response.StatusCode, method)
	}
	return nil
}

func (transport *httpTransport) close() error {
	transport.mu.Lock()
	session := transport.session
	transport.mu.Unlock()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, transport.endpoint, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Mcp-Session-Id", session)
	if transport.token != "" {
		request.Header.Set("Authorization", "Bearer "+transport.token)
	}
	if response, err := transport.client.Do(request); err == nil {
		response.Body.Close()
	}
	return nil
}
