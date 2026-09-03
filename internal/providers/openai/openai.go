package openai

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
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

const DefaultBaseURL = "https://api.openai.com"

type Options struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

type Provider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

func New(options Options) (*Provider, error) {
	if strings.TrimSpace(options.APIKey) == "" {
		return nil, errors.New("OpenAI API key is empty")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, errors.New("OpenAI model ID is empty")
	}
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid OpenAI base URL %q", options.BaseURL)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return nil, errors.New("OpenAI base URL must use HTTPS")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Minute}
	}
	return &Provider{apiKey: options.APIKey, endpoint: responsesEndpoint(options.BaseURL), model: options.Model, client: client}, nil
}

func (*Provider) Name() string { return "openai" }

func (*Provider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, ParallelToolCalls: true, StructuredOutput: true}, nil
}

func (provider *Provider) Stream(ctx context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	body, err := provider.requestBody(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OpenAI request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+provider.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &erkaruntime.ProviderError{Code: "connection_error", Message: "send OpenAI request: " + err.Error(), Retryable: true}
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		message := apiErrorMessage(data, http.StatusText(response.StatusCode))
		return nil, &erkaruntime.ProviderError{Code: fmt.Sprintf("http_%d", response.StatusCode), Message: message, Retryable: response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		response.Body.Close()
		return nil, fmt.Errorf("OpenAI API returned unexpected content type %q", contentType)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &stream{body: response.Body, scanner: scanner, calls: make(map[string]*streamCall)}, nil
}

type responseRequest struct {
	Model             string      `json:"model"`
	Instructions      string      `json:"instructions"`
	Input             []inputItem `json:"input"`
	Tools             []tool      `json:"tools,omitempty"`
	ToolChoice        string      `json:"tool_choice,omitempty"`
	ParallelToolCalls bool        `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int64       `json:"max_output_tokens"`
	Stream            bool        `json:"stream"`
	Store             bool        `json:"store"`
}

type inputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

func (provider *Provider) requestBody(request erkaruntime.ModelRequest) ([]byte, error) {
	input := make([]inputItem, 0, len(request.Items))
	for _, item := range request.Items {
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			input = append(input, inputItem{Role: "user", Content: item.Message.Content})
		case erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
			input = append(input, inputItem{Role: "assistant", Content: item.Message.Content})
		case erkaruntime.ItemToolCall:
			input = append(input, inputItem{Type: "function_call", CallID: string(item.ToolCall.CallID), Name: item.ToolCall.Tool, Arguments: string(item.ToolCall.Arguments)})
		case erkaruntime.ItemToolResult:
			input = append(input, inputItem{Type: "function_call_output", CallID: string(item.ToolResult.CallID), Output: item.ToolResult.Output})
		case erkaruntime.ItemCheckpoint:
			data, err := json.Marshal(item.Checkpoint)
			if err != nil {
				return nil, fmt.Errorf("encode checkpoint: %w", err)
			}
			input = append(input, inputItem{Role: "user", Content: "Durable stage checkpoint:\n" + string(data)})
		}
	}
	definitions := make([]tool, len(request.Tools))
	for index, definition := range request.Tools {
		definitions[index] = tool{Type: "function", Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema, Strict: false}
	}
	payload := responseRequest{Model: provider.model, Instructions: request.Instructions, Input: input, Tools: definitions, MaxOutputTokens: request.MaxOutputTokens, Stream: true, Store: false}
	if len(definitions) > 0 {
		payload.ToolChoice = "auto"
		payload.ParallelToolCalls = true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request: %w", err)
	}
	return data, nil
}

type streamCall struct {
	callID       erkaruntime.ToolCallID
	name         string
	sawArguments bool
}

type stream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	queue     []erkaruntime.ModelOutput
	calls     map[string]*streamCall
	completed bool
	closed    bool
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
		if string(data) == "[DONE]" {
			stream.completed = true
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelCompleted})
			break
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
		return nil, fmt.Errorf("read OpenAI stream: %w", err)
	}
	if len(lines) > 0 {
		return []byte(strings.Join(lines, "\n")), nil
	}
	return nil, io.EOF
}

func (stream *stream) translate(data []byte) error {
	var event struct {
		Type      string `json:"type"`
		Delta     string `json:"delta"`
		ItemID    string `json:"item_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Item      struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
		Response struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				InputDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode OpenAI stream event: %w", err)
	}
	switch event.Type {
	case "response.output_text.delta", "response.refusal.delta":
		if event.Delta != "" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: event.Delta})
		}
	case "response.output_item.added":
		if event.Item.Type == "function_call" {
			if event.Item.ID == "" || event.Item.CallID == "" || event.Item.Name == "" {
				return errors.New("OpenAI function call is missing item, call, or function identity")
			}
			call := &streamCall{callID: erkaruntime.ToolCallID(event.Item.CallID), name: event.Item.Name}
			stream.calls[event.Item.ID] = call
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: call.callID, Tool: call.name}})
		}
	case "response.function_call_arguments.delta":
		call := stream.calls[event.ItemID]
		if call == nil {
			return fmt.Errorf("OpenAI arguments reference unknown function item %q", event.ItemID)
		}
		if event.Delta != "" {
			call.sawArguments = true
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: call.callID, ArgumentsFragment: event.Delta}})
		}
	case "response.function_call_arguments.done":
		call := stream.calls[event.ItemID]
		if call == nil {
			return fmt.Errorf("OpenAI completed arguments reference unknown function item %q", event.ItemID)
		}
		if !call.sawArguments && event.Arguments != "" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: call.callID, ArgumentsFragment: event.Arguments}})
		}
	case "response.completed":
		if event.Response.Usage != nil {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelUsage, Usage: &erkaruntime.Usage{InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens, CacheReadTokens: event.Response.Usage.InputDetails.CachedTokens}})
		}
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelCompleted})
	case "response.failed":
		code, message := "provider_error", "OpenAI response failed"
		if event.Response.Error != nil {
			code, message = nonEmpty(event.Response.Error.Code, code), nonEmpty(event.Response.Error.Message, message)
		}
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: code, Message: message, Retryable: retryableCode(code)}})
	case "response.incomplete":
		reason := "unknown"
		if event.Response.IncompleteDetails != nil {
			reason = nonEmpty(event.Response.IncompleteDetails.Reason, reason)
		}
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: "incomplete", Message: "OpenAI response incomplete: " + reason, Retryable: reason == "server_error"}})
	case "error":
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: nonEmpty(event.Code, "stream_error"), Message: nonEmpty(event.Message, "OpenAI stream failed"), Retryable: retryableCode(event.Code)}})
	}
	return nil
}

func (stream *stream) Close() error {
	if stream.closed {
		return nil
	}
	stream.closed = true
	return stream.body.Close()
}

func responsesEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

func apiErrorMessage(data []byte, fallback string) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return payload.Error.Message
	}
	if value := strings.TrimSpace(string(data)); value != "" {
		return value
	}
	return fallback
}

func retryableCode(code string) bool {
	code = strings.ToLower(code)
	return strings.Contains(code, "rate") || strings.Contains(code, "timeout") || strings.Contains(code, "server") || strings.Contains(code, "overloaded") || strings.Contains(code, "unavailable")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

var _ erkaruntime.ModelProvider = (*Provider)(nil)
var _ erkaruntime.ModelStream = (*stream)(nil)
