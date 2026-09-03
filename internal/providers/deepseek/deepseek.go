package deepseek

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

const (
	DefaultBaseURL = "https://api.deepseek.com"
	DefaultModel   = "deepseek-v4-flash"
)

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
		return nil, errors.New("DeepSeek API key is empty")
	}
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid DeepSeek base URL %q", options.BaseURL)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return nil, errors.New("DeepSeek base URL must use HTTPS")
	}
	if options.Model == "" {
		options.Model = DefaultModel
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Minute}
	}
	return &Provider{
		apiKey: options.APIKey, endpoint: strings.TrimRight(options.BaseURL, "/") + "/chat/completions",
		model: options.Model, client: client,
	}, nil
}

func (*Provider) Name() string { return "deepseek" }

func (*Provider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true, ParallelToolCalls: true, StructuredOutput: true, ContextTokens: 1_000_000}, nil
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
		return nil, fmt.Errorf("create DeepSeek request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+provider.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &erkaruntime.ProviderError{Code: "connection_error", Message: "send DeepSeek request: " + err.Error(), Retryable: true}
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		message := strings.TrimSpace(string(data))
		var apiError struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &apiError) == nil && apiError.Error.Message != "" {
			message = apiError.Error.Message
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, &erkaruntime.ProviderError{Code: fmt.Sprintf("http_%d", response.StatusCode), Message: message, Retryable: response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		response.Body.Close()
		return nil, fmt.Errorf("DeepSeek API returned unexpected content type %q", contentType)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &stream{body: response.Body, scanner: scanner, callIDs: make(map[int]erkaruntime.ToolCallID)}, nil
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []message     `json:"messages"`
	Tools         []tool        `json:"tools,omitempty"`
	ToolChoice    string        `json:"tool_choice,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions streamOptions `json:"stream_options"`
	MaxTokens     int64         `json:"max_tokens"`
	Thinking      thinking      `json:"thinking"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}
type thinking struct {
	Type string `json:"type"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
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

func (provider *Provider) requestBody(request erkaruntime.ModelRequest) ([]byte, error) {
	messages := []message{{Role: "system", Content: request.Instructions}}
	for _, item := range request.Items {
		switch item.Kind {
		case erkaruntime.ItemUserMessage:
			messages = append(messages, message{Role: "user", Content: item.Message.Content})
		case erkaruntime.ItemAssistantMessage:
			messages = append(messages, message{Role: "assistant", Content: item.Message.Content})
		case erkaruntime.ItemReasoningSummary:
			messages = append(messages, message{Role: "assistant", Content: item.Message.Content})
		case erkaruntime.ItemToolCall:
			messages = append(messages, message{Role: "assistant", ToolCalls: []toolCall{{
				ID: string(item.ToolCall.CallID), Type: "function",
				Function: functionCall{Name: item.ToolCall.Tool, Arguments: string(item.ToolCall.Arguments)},
			}}})
		case erkaruntime.ItemToolResult:
			messages = append(messages, message{Role: "tool", Content: item.ToolResult.Output, ToolCallID: string(item.ToolResult.CallID)})
		case erkaruntime.ItemCheckpoint:
			data, err := json.Marshal(item.Checkpoint)
			if err != nil {
				return nil, fmt.Errorf("encode checkpoint: %w", err)
			}
			messages = append(messages, message{Role: "user", Content: "Durable stage checkpoint:\n" + string(data)})
		}
	}
	tools := make([]tool, len(request.Tools))
	for index, definition := range request.Tools {
		tools[index] = tool{Type: "function", Function: functionTool{Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema}}
	}
	payload := chatRequest{
		Model: provider.model, Messages: messages, Tools: tools, Stream: true,
		StreamOptions: streamOptions{IncludeUsage: true}, MaxTokens: request.MaxOutputTokens,
		Thinking: thinking{Type: "disabled"},
	}
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode DeepSeek request: %w", err)
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
		return nil, fmt.Errorf("read DeepSeek stream: %w", err)
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
			PromptTokens         int64 `json:"prompt_tokens"`
			CompletionTokens     int64 `json:"completion_tokens"`
			PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("decode DeepSeek stream event: %w", err)
	}
	if chunk.Error != nil {
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: nonEmpty(chunk.Error.Code, "provider_error"), Message: nonEmpty(chunk.Error.Message, "DeepSeek stream failed")}})
		return nil
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelTextDelta, Text: *choice.Delta.Content})
		}
		for _, delta := range choice.Delta.ToolCalls {
			callID := erkaruntime.ToolCallID(delta.ID)
			if callID != "" {
				stream.callIDs[delta.Index] = callID
			} else {
				callID = stream.callIDs[delta.Index]
			}
			if callID == "" {
				return fmt.Errorf("DeepSeek tool-call delta %d has no call ID", delta.Index)
			}
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: callID, Tool: delta.Function.Name, ArgumentsFragment: delta.Function.Arguments}})
		}
		if choice.FinishReason != nil && *choice.FinishReason != "stop" && *choice.FinishReason != "tool_calls" {
			stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelFailed, Error: &erkaruntime.RuntimeError{Code: "incomplete", Message: "DeepSeek stopped with reason " + *choice.FinishReason, Retryable: *choice.FinishReason == "insufficient_system_resource"}})
		}
	}
	if chunk.Usage != nil {
		stream.queue = append(stream.queue, erkaruntime.ModelOutput{Kind: erkaruntime.ModelUsage, Usage: &erkaruntime.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, CacheReadTokens: chunk.Usage.PromptCacheHitTokens}})
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

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ erkaruntime.ModelProvider = (*Provider)(nil)
var _ erkaruntime.ModelStream = (*stream)(nil)
