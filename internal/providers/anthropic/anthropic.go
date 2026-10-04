// Package anthropic implements a native provider for the Anthropic Messages
// API with prompt caching.
//
// Cache layout: the request is ordered tools → system → messages, which is
// the order the API hashes for caching. Tool definitions and the system
// prompt are serialized deterministically so the cached prefix is
// byte-identical across loop iterations. Three of the four allowed cache
// breakpoints are used: the last tool definition, the system prompt, and the
// final content block of the conversation, so each iteration reads the
// previous iteration's prefix from cache and writes only the new suffix.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const (
	Name           = "anthropic"
	DefaultBaseURL = "https://api.anthropic.com"
	APIVersion     = "2023-06-01"
	maxErrorBytes  = 4 << 10
)

type Options struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

type Provider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

func New(options Options) (*Provider, error) {
	if strings.TrimSpace(options.APIKey) == "" {
		return nil, errors.New("Anthropic API key is empty")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, errors.New("Anthropic model ID is empty")
	}
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Anthropic base URL %q", options.BaseURL)
	}
	if parsed.User != nil {
		return nil, errors.New("Anthropic base URL must not embed credentials")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return nil, errors.New("Anthropic base URL must use HTTPS")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Minute}
	}
	base := strings.TrimRight(options.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return &Provider{apiKey: options.APIKey, baseURL: base, model: options.Model, client: client}, nil
}

func (*Provider) Name() string { return Name }

func (*Provider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, ParallelToolCalls: true}, nil
}

func (provider *Provider) headers(request *http.Request) {
	request.Header.Set("x-api-key", provider.apiKey)
	request.Header.Set("anthropic-version", APIVersion)
}

func (provider *Provider) ListModels(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.baseURL+"/models?limit=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("create Anthropic model-list request: %w", err)
	}
	provider.headers(request)
	request.Header.Set("Accept", "application/json")
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("list Anthropic models: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read Anthropic model list: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("Anthropic model list exceeded 1 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return nil, httpError(response.StatusCode, data)
	}
	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode Anthropic model list: %w", err)
	}
	models := make([]erkaruntime.ModelInfo, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		model := erkaruntime.ModelInfo{ID: id, DisplayName: strings.TrimSpace(item.DisplayName), Default: id == provider.model}
		if model.Validate() != nil || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("Anthropic model list contained no valid models")
	}
	sort.Slice(models, func(left, right int) bool { return models[left].ID < models[right].ID })
	return models, nil
}

func (provider *Provider) Stream(ctx context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	body, err := provider.requestBody(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Anthropic request: %w", err)
	}
	provider.headers(httpRequest)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &erkaruntime.ProviderError{Code: "connection_error", Message: "send Anthropic request: " + err.Error(), Retryable: true}
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, httpError(response.StatusCode, data)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		response.Body.Close()
		return nil, fmt.Errorf("Anthropic API returned unexpected content type %q", contentType)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &stream{body: response.Body, scanner: scanner, blocks: make(map[int]*blockState)}, nil
}

func httpError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	code := fmt.Sprintf("http_%d", status)
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && strings.TrimSpace(payload.Error.Message) != "" {
		message = payload.Error.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	// 529 is Anthropic's "overloaded" status.
	retryable := status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	return &erkaruntime.ProviderError{Code: code, Message: message, Retryable: retryable}
}

type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

type messagesRequest struct {
	Model      string       `json:"model"`
	MaxTokens  int64        `json:"max_tokens"`
	Stream     bool         `json:"stream"`
	Tools      []tool       `json:"tools,omitempty"`
	ToolChoice *toolChoice  `json:"tool_choice,omitempty"`
	System     []systemText `json:"system"`
	Messages   []message    `json:"messages"`
}

type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type toolChoice struct {
	Type string `json:"type"`
}

type systemText struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

func (provider *Provider) requestBody(request erkaruntime.ModelRequest) ([]byte, error) {
	messages, err := buildMessages(request.Items)
	if err != nil {
		return nil, err
	}
	payload := messagesRequest{
		Model: provider.model, MaxTokens: request.MaxOutputTokens, Stream: true,
		System:   []systemText{{Type: "text", Text: request.Instructions, CacheControl: ephemeral}},
		Messages: messages,
	}
	if len(request.Tools) > 0 {
		payload.Tools = make([]tool, len(request.Tools))
		for index, definition := range request.Tools {
			schema, err := compactJSON(definition.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("tool %q schema: %w", definition.Name, err)
			}
			payload.Tools[index] = tool{Name: definition.Name, Description: definition.Description, InputSchema: schema}
		}
		payload.Tools[len(payload.Tools)-1].CacheControl = ephemeral
		payload.ToolChoice = &toolChoice{Type: "auto"}
	}
	if count := len(payload.Messages); count > 0 {
		last := &payload.Messages[count-1]
		last.Content[len(last.Content)-1].CacheControl = ephemeral
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic request: %w", err)
	}
	return data, nil
}

// buildMessages converts durable runtime items into strictly alternating
// user/assistant turns. Consecutive same-role content is merged into one
// message and tool results are returned inside user turns.
func buildMessages(items []erkaruntime.RuntimeItem) ([]message, error) {
	var messages []message
	add := func(role string, block contentBlock) {
		if count := len(messages); count > 0 && messages[count-1].Role == role {
			messages[count-1].Content = append(messages[count-1].Content, block)
			return
		}
		messages = append(messages, message{Role: role, Content: []contentBlock{block}})
	}
	for _, item := range items {
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			if strings.TrimSpace(item.Message.Content) != "" {
				add("user", contentBlock{Type: "text", Text: item.Message.Content})
			}
		case erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
			if strings.TrimSpace(item.Message.Content) != "" {
				add("assistant", contentBlock{Type: "text", Text: item.Message.Content})
			}
		case erkaruntime.ItemToolCall:
			input := json.RawMessage(`{}`)
			if compact, err := compactJSON(item.ToolCall.Arguments); err == nil && len(compact) > 0 && compact[0] == '{' {
				input = compact
			}
			add("assistant", contentBlock{Type: "tool_use", ID: string(item.ToolCall.CallID), Name: item.ToolCall.Tool, Input: input})
		case erkaruntime.ItemToolResult:
			output := item.ToolResult.Output
			if output == "" {
				output = "(no output)"
			}
			add("user", contentBlock{Type: "tool_result", ToolUseID: string(item.ToolResult.CallID), Content: output, IsError: item.ToolResult.IsError})
		case erkaruntime.ItemCheckpoint:
			data, err := json.Marshal(item.Checkpoint)
			if err != nil {
				return nil, fmt.Errorf("encode checkpoint: %w", err)
			}
			add("user", contentBlock{Type: "text", Text: "Durable stage checkpoint:\n" + string(data)})
		}
	}
	if len(messages) == 0 || messages[0].Role != "user" {
		messages = append([]message{{Role: "user", Content: []contentBlock{{Type: "text", Text: "Continue."}}}}, messages...)
	}
	return messages, nil
}

func compactJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty JSON")
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type blockState struct {
	toolCall     bool
	callID       erkaruntime.ToolCallID
	sawArguments bool
}

type usageCounts struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type stream struct {
	body       io.ReadCloser
	scanner    *bufio.Scanner
	queue      []erkaruntime.ModelOutput
	blocks     map[int]*blockState
	usage      usageCounts
	stopReason string
	completed  bool
	closed     bool
}

func (stream *stream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	for len(stream.queue) == 0 {
		if err := ctx.Err(); err != nil {
			return erkaruntime.ModelOutput{}, err
		}
		if stream.completed {
			return erkaruntime.ModelOutput{}, io.EOF
		}
		data, err := stream.nextEvent()
		if err != nil {
			return erkaruntime.ModelOutput{}, err
		}
		if err := stream.translate(data); err != nil {
			return erkaruntime.ModelOutput{}, err
		}
	}
	output := stream.queue[0]
	stream.queue = stream.queue[1:]
	return output, nil
}

func (stream *stream) nextEvent() ([]byte, error) {
	var lines []string
	for stream.scanner.Scan() {
		line := stream.scanner.Text()
		if line == "" {
			if len(lines) > 0 {
				return []byte(strings.Join(lines, "\n")), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := stream.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Anthropic stream: %w", err)
	}
	if len(lines) > 0 {
		return []byte(strings.Join(lines, "\n")), nil
	}
	return nil, fmt.Errorf("%w: Anthropic stream ended before message_stop", erkaruntime.ErrMalformedStream)
}

func (stream *stream) translate(data []byte) error {
	var event struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage usageCounts `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *usageCounts `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode Anthropic stream event: %w", err)
	}
	switch event.Type {
	case "message_start":
		stream.usage = event.Message.Usage
	case "content_block_start":
		state := &blockState{}
		if event.ContentBlock.Type == "tool_use" {
			if event.ContentBlock.ID == "" || event.ContentBlock.Name == "" {
				return errors.New("Anthropic tool_use block is missing its ID or name")
			}
			state.toolCall, state.callID = true, erkaruntime.ToolCallID(event.ContentBlock.ID)
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: state.callID, Tool: event.ContentBlock.Name}})
		}
		stream.blocks[event.Index] = state
	case "content_block_delta":
		state := stream.blocks[event.Index]
		if state == nil {
			return fmt.Errorf("Anthropic delta references unknown content block %d", event.Index)
		}
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: event.Delta.Text})
			}
		case "input_json_delta":
			if !state.toolCall {
				return fmt.Errorf("Anthropic input_json_delta for non-tool block %d", event.Index)
			}
			if event.Delta.PartialJSON != "" {
				state.sawArguments = true
				stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: state.callID, ArgumentsFragment: event.Delta.PartialJSON}})
			}
		}
	case "content_block_stop":
		if state := stream.blocks[event.Index]; state != nil && state.toolCall && !state.sawArguments {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: state.callID, ArgumentsFragment: "{}"}})
		}
	case "message_delta":
		if event.Delta.StopReason != "" {
			stream.stopReason = event.Delta.StopReason
		}
		if event.Usage != nil {
			// message_delta usage is cumulative; prefer non-zero values.
			mergeUsage(&stream.usage, *event.Usage)
		}
	case "message_stop":
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelUsage, Usage: normalizedUsage(stream.usage)})
		switch stream.stopReason {
		case "", "end_turn", "tool_use", "stop_sequence":
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelCompleted})
		default:
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: "incomplete", Message: "Anthropic model stopped with reason " + stream.stopReason}})
		}
	case "error":
		code := nonEmpty(event.Error.Type, "stream_error")
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: code, Message: nonEmpty(event.Error.Message, "Anthropic stream failed"), Retryable: code == "overloaded_error" || code == "rate_limit_error" || code == "api_error"}})
	}
	return nil
}

func mergeUsage(total *usageCounts, next usageCounts) {
	if next.InputTokens > 0 {
		total.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		total.OutputTokens = next.OutputTokens
	}
	if next.CacheCreationInputTokens > 0 {
		total.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.CacheReadInputTokens > 0 {
		total.CacheReadInputTokens = next.CacheReadInputTokens
	}
}

// normalizedUsage reports InputTokens as the full prompt size, matching the
// OpenAI and DeepSeek providers whose input counts include cached tokens.
func normalizedUsage(counts usageCounts) *erkaruntime.Usage {
	return &erkaruntime.Usage{
		InputTokens:      counts.InputTokens + counts.CacheCreationInputTokens + counts.CacheReadInputTokens,
		OutputTokens:     counts.OutputTokens,
		CacheReadTokens:  counts.CacheReadInputTokens,
		CacheWriteTokens: counts.CacheCreationInputTokens,
	}
}

func (stream *stream) Close() error {
	if stream.closed {
		return nil
	}
	stream.closed = true
	return stream.body.Close()
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

var _ erkaruntime.ModelProvider = (*Provider)(nil)
var _ erkaruntime.ModelCatalog = (*Provider)(nil)
var _ erkaruntime.ModelStream = (*stream)(nil)
