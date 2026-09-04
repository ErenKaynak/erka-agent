package client

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
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20

type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (err *APIError) Error() string {
	if err.Code == "" {
		return fmt.Sprintf("remote API returned HTTP %d", err.Status)
	}
	return fmt.Sprintf("remote API %s: %s", err.Code, err.Message)
}

type Project struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	RepositoryRoot string    `json:"repositoryRoot"`
	DefaultRef     string    `json:"defaultRef"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Task struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	Objective         string    `json:"objective"`
	Pipeline          string    `json:"pipeline"`
	Status            string    `json:"status"`
	RuntimeStatus     string    `json:"runtimeStatus"`
	LastItemSequence  uint64    `json:"lastItemSequence"`
	LastEventSequence uint64    `json:"lastEventSequence"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type Event struct {
	ID       string          `json:"id"`
	Sequence uint64          `json:"sequence"`
	TaskID   string          `json:"taskId"`
	RunID    string          `json:"runId"`
	Type     string          `json:"type"`
	Time     time.Time       `json:"time"`
	Message  string          `json:"message"`
	Data     json.RawMessage `json:"data"`
}

type Approval struct {
	ID          string     `json:"id"`
	TaskID      string     `json:"taskId"`
	Kind        string     `json:"kind"`
	Message     string     `json:"message"`
	Status      string     `json:"status"`
	Actor       string     `json:"actor"`
	RequestedAt time.Time  `json:"requestedAt"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	DecidedAt   *time.Time `json:"decidedAt"`
}

func New(baseURL, token string, transport *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid remote server URL %q", baseURL)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, errors.New("remote server URL must use HTTPS unless it targets loopback")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("remote server token is empty")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if transport == nil {
		transport = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: parsed, token: token, http: transport}, nil
}

func (client *Client) Projects(ctx context.Context) ([]Project, error) {
	var response struct {
		Projects []Project `json:"projects"`
	}
	if err := client.doJSON(ctx, http.MethodGet, "/v1/projects", nil, &response); err != nil {
		return nil, err
	}
	return response.Projects, nil
}

func (client *Client) Run(ctx context.Context, projectID, objective string) (Task, error) {
	var task Task
	err := client.doJSON(ctx, http.MethodPost, "/v1/tasks", map[string]string{"projectId": projectID, "objective": objective}, &task)
	return task, err
}

func (client *Client) Task(ctx context.Context, taskID string) (Task, error) {
	var task Task
	err := client.doJSON(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID), nil, &task)
	return task, err
}

func (client *Client) Diff(ctx context.Context, taskID string) (string, error) {
	var response struct {
		Diff string `json:"diff"`
	}
	err := client.doJSON(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/diff", nil, &response)
	return response.Diff, err
}

func (client *Client) Cancel(ctx context.Context, taskID string) error {
	return client.doJSON(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/cancel", nil, nil)
}

func (client *Client) Retry(ctx context.Context, taskID string) error {
	return client.doJSON(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(taskID)+"/retry", nil, nil)
}

func (client *Client) DecideApproval(ctx context.Context, approvalID, decision string) (Approval, error) {
	var approval Approval
	err := client.doJSON(ctx, http.MethodPost, "/v1/approvals/"+url.PathEscape(approvalID)+"/decision", map[string]string{"decision": decision}, &approval)
	return approval, err
}

func (client *Client) Watch(ctx context.Context, taskID, lastEventID string, receive func(Event) error) error {
	if receive == nil {
		return errors.New("remote event receiver is nil")
	}
	request, err := client.request(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/events/stream", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	if strings.TrimSpace(lastEventID) != "" {
		request.Header.Set("Last-Event-ID", strings.TrimSpace(lastEventID))
	}
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeAPIError(response)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maxResponseBytes))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var event Event
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
				return fmt.Errorf("decode remote event: %w", err)
			}
			if event.ID == "" || event.Sequence == 0 || event.TaskID != taskID || event.Type == "" {
				return errors.New("remote event envelope is invalid")
			}
			if err := receive(event); err != nil {
				return err
			}
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read remote event stream: %w", err)
	}
	if data.Len() != 0 {
		return errors.New("remote event stream ended mid-event")
	}
	return nil
}

func (client *Client) doJSON(ctx context.Context, method, path string, input, output any) error {
	request, err := client.request(ctx, method, path, input)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}
	if output == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
		return err
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode remote response: %w", err)
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return errors.New("remote response contains trailing JSON")
	}
	return nil
}

func (client *Client) request(ctx context.Context, method, path string, input any) (*http.Request, error) {
	target := *client.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("User-Agent", "erka-remote/v1")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	} else if method == http.MethodPost {
		request.Body = io.NopCloser(strings.NewReader(""))
		request.ContentLength = 0
	}
	return request, nil
}

func decodeAPIError(response *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	return &APIError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func AfterSequence(sequence uint64) string { return strconv.FormatUint(sequence, 10) }
