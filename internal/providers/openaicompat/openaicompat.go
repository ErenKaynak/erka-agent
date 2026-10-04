// Package openaicompat implements a native provider for servers that speak
// the OpenAI Chat Completions streaming protocol, such as OpenRouter, Ollama,
// vLLM, LM Studio, and llama.cpp.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// Name is the provider name used in configuration.
const Name = "openai_compatible"

const maxErrorBytes = 4 << 10

type Options struct {
	// APIKey is optional; when empty no Authorization header is sent.
	APIKey string
	// BaseURL is the server root including any version path, for example
	// https://openrouter.ai/api/v1 or http://localhost:11434/v1.
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
	if strings.TrimSpace(options.Model) == "" {
		return nil, errors.New("OpenAI-compatible model ID is empty")
	}
	if strings.TrimSpace(options.BaseURL) == "" {
		return nil, errors.New("OpenAI-compatible base URL is empty")
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid OpenAI-compatible base URL %q", options.BaseURL)
	}
	if parsed.User != nil {
		return nil, errors.New("OpenAI-compatible base URL must not embed credentials")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, errors.New("OpenAI-compatible base URL must use HTTPS unless it targets a loopback address")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Minute}
	}
	return &Provider{apiKey: options.APIKey, baseURL: strings.TrimRight(options.BaseURL, "/"), model: options.Model, client: client}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (*Provider) Name() string { return Name }

func (*Provider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, ParallelToolCalls: true}, nil
}

func (provider *Provider) authorize(request *http.Request) {
	if provider.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+provider.apiKey)
	}
}

func (provider *Provider) ListModels(ctx context.Context) ([]erkaruntime.ModelInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create model-list request: %w", err)
	}
	provider.authorize(request)
	request.Header.Set("Accept", "application/json")
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("list OpenAI-compatible models: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read model list: %w", err)
	}
	if len(data) > 4<<20 {
		return nil, errors.New("OpenAI-compatible model list exceeded 4 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return nil, httpError(response.StatusCode, data)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	models := make([]erkaruntime.ModelInfo, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		model := erkaruntime.ModelInfo{ID: id, Default: id == provider.model}
		if model.Validate() != nil || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("OpenAI-compatible model list contained no valid models")
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
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OpenAI-compatible request: %w", err)
	}
	provider.authorize(httpRequest)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &erkaruntime.ProviderError{Code: "connection_error", Message: "send OpenAI-compatible request: " + err.Error(), Retryable: true}
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, httpError(response.StatusCode, data)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		response.Body.Close()
		return nil, fmt.Errorf("OpenAI-compatible server returned unexpected content type %q", contentType)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &stream{body: response.Body, scanner: scanner, callIDs: make(map[int]erkaruntime.ToolCallID)}, nil
}

func httpError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	var payload struct {
		Error struct {
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
	return &erkaruntime.ProviderError{Code: fmt.Sprintf("http_%d", status), Message: message, Retryable: status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500}
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []message     `json:"messages"`
	Tools         []tool        `json:"tools,omitempty"`
	ToolChoice    string        `json:"tool_choice,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions streamOptions `json:"stream_options"`
	MaxTokens     int64         `json:"max_tokens"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function functionTool `json:"function"`
}

type functionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func text(value string) *string { return &value }

// requestBody groups an assistant message and the tool calls that follow it
// into one assistant turn, because strict Chat Completions servers require
// every tool result to answer a tool call of the immediately preceding
// assistant message.
func (provider *Provider) requestBody(request erkaruntime.ModelRequest) ([]byte, error) {
	messages := []message{{Role: "system", Content: text(request.Instructions)}}
	appendAssistant := func(content *string, call *toolCall) {
		last := &messages[len(messages)-1]
		if last.Role == "assistant" && (call != nil || last.ToolCalls == nil) {
			if content != nil {
				if last.Content == nil {
					last.Content = content
				} else {
					last.Content = text(*last.Content + "\n" + *content)
				}
			}
			if call != nil {
				last.ToolCalls = append(last.ToolCalls, *call)
			}
			return
		}
		next := message{Role: "assistant", Content: content}
		if call != nil {
			next.ToolCalls = []toolCall{*call}
		}
		messages = append(messages, next)
	}
	for _, item := range request.Items {
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			messages = append(messages, message{Role: "user", Content: text(item.Message.Content)})
		case erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
			appendAssistant(text(item.Message.Content), nil)
		case erkaruntime.ItemToolCall:
			appendAssistant(nil, &toolCall{
				ID: string(item.ToolCall.CallID), Type: "function",
				Function: functionCall{Name: item.ToolCall.Tool, Arguments: string(item.ToolCall.Arguments)},
			})
		case erkaruntime.ItemToolResult:
			messages = append(messages, message{Role: "tool", Content: text(item.ToolResult.Output), ToolCallID: string(item.ToolResult.CallID)})
		case erkaruntime.ItemCheckpoint:
			data, err := json.Marshal(item.Checkpoint)
			if err != nil {
				return nil, fmt.Errorf("encode checkpoint: %w", err)
			}
			messages = append(messages, message{Role: "user", Content: text("Durable stage checkpoint:\n" + string(data))})
		}
	}
	tools := make([]tool, len(request.Tools))
	for index, definition := range request.Tools {
		tools[index] = tool{Type: "function", Function: functionTool{Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema}}
	}
	payload := chatRequest{Model: provider.model, Messages: messages, Tools: tools, Stream: true, StreamOptions: streamOptions{IncludeUsage: true}, MaxTokens: request.MaxOutputTokens}
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI-compatible request: %w", err)
	}
	return data, nil
}

type stream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	queue     []erkaruntime.ModelOutput
	callIDs   map[int]erkaruntime.ToolCallID
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
		return nil, fmt.Errorf("read OpenAI-compatible stream: %w", err)
	}
	if len(lines) > 0 {
		return []byte(strings.Join(lines, "\n")), nil
	}
	return nil, io.EOF
}

func (stream *stream) translate(data []byte) error {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   *string `json:"content"`
				Refusal   *string `json:"refusal"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("decode OpenAI-compatible stream event: %w", err)
	}
	if chunk.Error != nil {
		code := "provider_error"
		if value, ok := chunk.Error.Code.(string); ok && value != "" {
			code = value
		}
		stream.completed = true
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: code, Message: nonEmpty(chunk.Error.Message, "OpenAI-compatible stream failed"), Retryable: retryableCode(code)}})
		return nil
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: *choice.Delta.Content})
		}
		if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: *choice.Delta.Refusal})
		}
		for _, delta := range choice.Delta.ToolCalls {
			callID := erkaruntime.ToolCallID(delta.ID)
			if callID != "" {
				stream.callIDs[delta.Index] = callID
			} else {
				callID = stream.callIDs[delta.Index]
			}
			if callID == "" {
				return fmt.Errorf("OpenAI-compatible tool-call delta %d has no call ID", delta.Index)
			}
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: callID, Tool: delta.Function.Name, ArgumentsFragment: delta.Function.Arguments}})
		}
		if choice.FinishReason != nil {
			switch *choice.FinishReason {
			case "", "stop", "tool_calls", "function_call":
			default:
				stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: "incomplete", Message: "OpenAI-compatible model stopped with reason " + *choice.FinishReason}})
			}
		}
	}
	if chunk.Usage != nil {
		usage := &erkaruntime.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelUsage, Usage: usage})
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
var _ erkaruntime.ModelCatalog = (*Provider)(nil)
var _ erkaruntime.ModelStream = (*stream)(nil)
