package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const maxRequestBody = 1 << 20

type APIStore interface {
	AuthenticateAPIToken(context.Context, [32]byte, string) (store.TokenPrincipal, error)
	ListServerProjects(context.Context) ([]store.ServerProject, error)
	LoadServerProject(context.Context, string) (store.ServerProject, error)
	ListServerTasks(context.Context, string) ([]store.ServerTask, error)
	LoadServerTask(context.Context, erkaruntime.TaskID) (store.ServerTask, error)
	ListApprovals(context.Context, erkaruntime.TaskID, store.ApprovalStatus) ([]store.Approval, error)
	LoadApproval(context.Context, string) (store.Approval, error)
	DecideApproval(context.Context, string, string, store.ApprovalStatus, time.Time) error
	LoadTask(context.Context, erkaruntime.TaskID) (erkaruntime.TaskSnapshot, error)
	LoadItems(context.Context, erkaruntime.TaskID, uint64) ([]erkaruntime.RuntimeItem, error)
	LoadEvents(context.Context, erkaruntime.TaskID, uint64) ([]erkaruntime.Event, error)
	LoadEventSequence(context.Context, erkaruntime.TaskID, erkaruntime.EventID) (uint64, error)
}

type ResourceLoader func(context.Context) (any, error)

type APIOptions struct {
	Service      *Service
	Store        APIStore
	Clock        func() time.Time
	SSEPoll      time.Duration
	SSEHeartbeat time.Duration
	Connections  ResourceLoader
	Models       ResourceLoader
	Pipelines    ResourceLoader
	Previews     *PreviewManager
	Memories     MemoryAPIStore
}

type API struct {
	service      *Service
	store        APIStore
	clock        func() time.Time
	ssePoll      time.Duration
	sseHeartbeat time.Duration
	connections  ResourceLoader
	models       ResourceLoader
	pipelines    ResourceLoader
	previews     *PreviewManager
	memories     MemoryAPIStore
	handler      http.Handler
}

type principalContextKey struct{}

func NewAPI(options APIOptions) (*API, error) {
	if options.Service == nil || options.Store == nil {
		return nil, errors.New("server API requires service and store")
	}
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.SSEPoll <= 0 {
		options.SSEPoll = 250 * time.Millisecond
	}
	if options.SSEHeartbeat <= 0 {
		options.SSEHeartbeat = 15 * time.Second
	}
	empty := func(context.Context) (any, error) { return []any{}, nil }
	if options.Connections == nil {
		options.Connections = empty
	}
	if options.Models == nil {
		options.Models = empty
	}
	if options.Pipelines == nil {
		options.Pipelines = empty
	}
	api := &API{
		service: options.Service, store: options.Store, clock: options.Clock,
		ssePoll: options.SSEPoll, sseHeartbeat: options.SSEHeartbeat,
		connections: options.Connections, models: options.Models, pipelines: options.Pipelines,
		previews: options.Previews, memories: options.Memories,
	}
	api.handler = api.routes()
	return api, nil
}

func (api *API) Handler() http.Handler { return api.handler }

func (api *API) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", api.health)
	mux.Handle("GET /app/", webappHandler())
	mux.Handle("GET /{$}", http.RedirectHandler("/app/", http.StatusFound))
	mux.Handle("GET /v1/me", api.authenticate("read", http.HandlerFunc(api.me)))
	mux.Handle("GET /v1/projects", api.authenticate("read", http.HandlerFunc(api.listProjects)))
	mux.Handle("POST /v1/projects", api.authenticate("write", http.HandlerFunc(api.createProject)))
	mux.Handle("GET /v1/projects/{id}", api.authenticate("read", http.HandlerFunc(api.getProject)))
	mux.Handle("PATCH /v1/projects/{id}", api.authenticate("write", http.HandlerFunc(api.updateProject)))
	mux.Handle("GET /v1/tasks", api.authenticate("read", http.HandlerFunc(api.listTasks)))
	mux.Handle("POST /v1/tasks", api.authenticate("write", http.HandlerFunc(api.createTask)))
	mux.Handle("GET /v1/tasks/{id}", api.authenticate("read", http.HandlerFunc(api.getTask)))
	mux.Handle("POST /v1/tasks/{id}/messages", api.authenticate("write", http.HandlerFunc(api.addMessage)))
	mux.Handle("POST /v1/tasks/{id}/cancel", api.authenticate("write", http.HandlerFunc(api.cancelTask)))
	mux.Handle("POST /v1/tasks/{id}/retry", api.authenticate("write", http.HandlerFunc(api.retryTask)))
	mux.Handle("GET /v1/tasks/{id}/items", api.authenticate("read", http.HandlerFunc(api.listItems)))
	mux.Handle("GET /v1/tasks/{id}/events", api.authenticate("read", http.HandlerFunc(api.listEvents)))
	mux.Handle("GET /v1/tasks/{id}/events/stream", api.authenticate("read", http.HandlerFunc(api.streamEvents)))
	mux.Handle("GET /v1/tasks/{id}/diff", api.authenticate("read", http.HandlerFunc(api.getDiff)))
	mux.Handle("GET /v1/approvals", api.authenticate("read", http.HandlerFunc(api.listApprovals)))
	mux.Handle("POST /v1/approvals/{id}/decision", api.authenticate("approve", http.HandlerFunc(api.decideApproval)))
	mux.Handle("GET /v1/connections", api.authenticate("read", api.resource(api.connections)))
	mux.Handle("GET /v1/models", api.authenticate("read", api.resource(api.models)))
	mux.Handle("GET /v1/pipelines", api.authenticate("read", api.resource(api.pipelines)))
	api.resourceRoutes(mux)
	mux.Handle("/", api.authenticate("read", http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeAPIError(response, http.StatusNotFound, "not_found", "API route not found")
	})))
	return mux
}

func (api *API) authenticate(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorization := strings.TrimSpace(request.Header.Get("Authorization"))
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
			response.Header().Set("WWW-Authenticate", `Bearer realm="erka"`)
			writeAPIError(response, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
			return
		}
		hash := sha256.Sum256([]byte(parts[1]))
		principal, err := api.store.AuthenticateAPIToken(request.Context(), hash, scope)
		if err != nil {
			response.Header().Set("WWW-Authenticate", `Bearer realm="erka"`)
			writeAPIError(response, http.StatusUnauthorized, "unauthorized", "The bearer token is invalid or lacks the required scope")
			return
		}
		ctx := context.WithValue(request.Context(), principalContextKey{}, principal)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

func (api *API) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{"status": "ok", "time": api.clock().UTC()})
}

func (api *API) me(response http.ResponseWriter, request *http.Request) {
	principal, _ := request.Context().Value(principalContextKey{}).(store.TokenPrincipal)
	writeJSON(response, http.StatusOK, principalDTO{ID: principal.ID, Name: principal.Name, Scopes: principal.Scopes})
}

func (api *API) listProjects(response http.ResponseWriter, request *http.Request) {
	projects, err := api.store.ListServerProjects(request.Context())
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]projectDTO, len(projects))
	for index, project := range projects {
		result[index] = projectResponse(project)
	}
	writeJSON(response, http.StatusOK, map[string]any{"projects": result})
}

func (api *API) createProject(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Name           string `json:"name"`
		RepositoryRoot string `json:"repositoryRoot"`
		DefaultRef     string `json:"defaultRef"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	project, err := api.service.RegisterProject(request.Context(), input.Name, input.RepositoryRoot, input.DefaultRef)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_project", err.Error())
		return
	}
	writeJSON(response, http.StatusCreated, projectResponse(project))
}

func (api *API) getProject(response http.ResponseWriter, request *http.Request) {
	project, err := api.store.LoadServerProject(request.Context(), request.PathValue("id"))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, projectResponse(project))
}

func (api *API) updateProject(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Name       *string `json:"name"`
		DefaultRef *string `json:"defaultRef"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.Name == nil && input.DefaultRef == nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", "name or defaultRef is required")
		return
	}
	project, err := api.service.UpdateProject(request.Context(), request.PathValue("id"), input.Name, input.DefaultRef)
	if err != nil {
		if errors.Is(err, erkaruntime.ErrNotFound) {
			writeMappedError(response, err)
		} else {
			writeAPIError(response, http.StatusBadRequest, "invalid_project", err.Error())
		}
		return
	}
	writeJSON(response, http.StatusOK, projectResponse(project))
}

func (api *API) listTasks(response http.ResponseWriter, request *http.Request) {
	tasks, err := api.store.ListServerTasks(request.Context(), strings.TrimSpace(request.URL.Query().Get("projectId")))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]taskDTO, len(tasks))
	for index, task := range tasks {
		result[index] = api.taskResponse(request.Context(), task)
	}
	writeJSON(response, http.StatusOK, map[string]any{"tasks": result})
}

func (api *API) createTask(response http.ResponseWriter, request *http.Request) {
	var input struct {
		ProjectID string `json:"projectId"`
		Objective string `json:"objective"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(input.ProjectID) == "" || strings.TrimSpace(input.Objective) == "" {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", "projectId and objective are required")
		return
	}
	task, err := api.service.SubmitTask(request.Context(), input.ProjectID, input.Objective)
	if err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, api.taskResponse(request.Context(), task))
}

func (api *API) getTask(response http.ResponseWriter, request *http.Request) {
	task, err := api.store.LoadServerTask(request.Context(), erkaruntime.TaskID(request.PathValue("id")))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, api.taskResponse(request.Context(), task))
}

func (api *API) addMessage(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Prompt string `json:"prompt"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(input.Prompt) == "" {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if err := api.service.AddMessage(request.Context(), erkaruntime.TaskID(request.PathValue("id")), input.Prompt); err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{"status": "queued"})
}

func (api *API) cancelTask(response http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(response, request); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := api.service.CancelTask(request.Context(), erkaruntime.TaskID(request.PathValue("id"))); err != nil {
		writeMappedError(response, err)
		return
	}
	api.previews.StopTask(erkaruntime.TaskID(request.PathValue("id")))
	writeJSON(response, http.StatusAccepted, map[string]any{"status": "cancelling"})
}

func (api *API) retryTask(response http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(response, request); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := api.service.RetryTask(request.Context(), erkaruntime.TaskID(request.PathValue("id"))); err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{"status": "queued"})
}

func (api *API) listItems(response http.ResponseWriter, request *http.Request) {
	after, err := parseAfter(request.URL.Query().Get("after"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	items, err := api.store.LoadItems(request.Context(), erkaruntime.TaskID(request.PathValue("id")), after)
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]itemDTO, len(items))
	for index, item := range items {
		result[index] = itemResponse(item)
	}
	writeJSON(response, http.StatusOK, map[string]any{"items": result})
}

func (api *API) listEvents(response http.ResponseWriter, request *http.Request) {
	after, err := parseAfter(request.URL.Query().Get("after"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	events, err := api.store.LoadEvents(request.Context(), erkaruntime.TaskID(request.PathValue("id")), after)
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]eventDTO, len(events))
	for index, event := range events {
		result[index] = eventResponse(event)
	}
	writeJSON(response, http.StatusOK, map[string]any{"events": result})
}

func (api *API) streamEvents(response http.ResponseWriter, request *http.Request) {
	taskID := erkaruntime.TaskID(request.PathValue("id"))
	if _, err := api.store.LoadServerTask(request.Context(), taskID); err != nil {
		writeMappedError(response, err)
		return
	}
	cursor, err := api.eventCursor(request, taskID)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeAPIError(response, http.StatusInternalServerError, "stream_unsupported", "Streaming is unavailable")
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()
	poll := time.NewTicker(api.ssePoll)
	heartbeat := time.NewTicker(api.sseHeartbeat)
	defer poll.Stop()
	defer heartbeat.Stop()
	for {
		events, loadErr := api.store.LoadEvents(request.Context(), taskID, cursor)
		if loadErr != nil {
			return
		}
		for _, event := range events {
			encoded, marshalErr := json.Marshal(eventResponse(event))
			if marshalErr != nil {
				return
			}
			if _, writeErr := fmt.Fprintf(response, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Type, encoded); writeErr != nil {
				return
			}
			cursor = event.Sequence
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		task, taskErr := api.store.LoadServerTask(request.Context(), taskID)
		if taskErr != nil {
			return
		}
		if terminalServerTask(task.Status) && len(events) == 0 {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-poll.C:
		case <-heartbeat.C:
			if _, err := io.WriteString(response, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (api *API) getDiff(response http.ResponseWriter, request *http.Request) {
	diff, err := api.service.Diff(request.Context(), erkaruntime.TaskID(request.PathValue("id")))
	if err != nil {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"diff": diff})
}

func (api *API) listApprovals(response http.ResponseWriter, request *http.Request) {
	status := store.ApprovalStatus(strings.TrimSpace(request.URL.Query().Get("status")))
	if status != "" && status != store.ApprovalPending && status != store.ApprovalApproved && status != store.ApprovalRejected && status != store.ApprovalExpired {
		writeAPIError(response, http.StatusBadRequest, "invalid_status", "Unknown approval status")
		return
	}
	approvals, err := api.store.ListApprovals(request.Context(), erkaruntime.TaskID(strings.TrimSpace(request.URL.Query().Get("taskId"))), status)
	if err != nil {
		writeMappedError(response, err)
		return
	}
	result := make([]approvalDTO, len(approvals))
	for index, approval := range approvals {
		result[index] = approvalResponse(approval)
	}
	writeJSON(response, http.StatusOK, map[string]any{"approvals": result})
}

func (api *API) decideApproval(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Decision store.ApprovalStatus `json:"decision"`
		Response string               `json:"response"`
		Scope    store.ApprovalScope  `json:"scope"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.Decision != store.ApprovalApproved && input.Decision != store.ApprovalRejected {
		writeAPIError(response, http.StatusBadRequest, "invalid_decision", "Decision must be approved or rejected")
		return
	}
	principal, _ := request.Context().Value(principalContextKey{}).(store.TokenPrincipal)
	actor := principal.Name
	if actor == "" {
		actor = principal.ID
	}
	if input.Scope != "" && input.Scope != store.ApprovalScopeOnce && input.Scope != store.ApprovalScopeTask {
		writeAPIError(response, http.StatusBadRequest, "invalid_scope", "Scope must be once or task")
		return
	}
	approval, err := api.service.DecideApproval(request.Context(), store.ApprovalDecision{ID: request.PathValue("id"), Actor: actor, Status: input.Decision, Response: input.Response, Scope: input.Scope, Time: api.clock().UTC()})
	if err != nil && approval.ID == "" {
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, approvalResponse(approval))
}

func (api *API) resource(loader ResourceLoader) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		value, err := loader(request.Context())
		if err != nil {
			writeMappedError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, value)
	})
}

func (api *API) eventCursor(request *http.Request, taskID erkaruntime.TaskID) (uint64, error) {
	cursor := strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	if cursor == "" {
		return parseAfter(request.URL.Query().Get("after"))
	}
	if sequence, err := strconv.ParseUint(cursor, 10, 64); err == nil {
		return sequence, nil
	}
	return api.store.LoadEventSequence(request.Context(), taskID, erkaruntime.EventID(cursor))
}

func (api *API) taskResponse(ctx context.Context, task store.ServerTask) taskDTO {
	result := taskDTO{
		ID: string(task.TaskID), ProjectID: task.ServerProjectID, Status: string(task.Status),
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		Outcome: string(task.Outcome), ResultCommit: task.ResultCommit,
	}
	if !task.FinalizedAt.IsZero() {
		finalized := task.FinalizedAt
		result.FinalizedAt = &finalized
		if task.Outcome == store.TaskAccepted {
			result.ResultBranch = task.ResultRef
		} else {
			result.ResultRef = task.ResultRef
		}
	}
	if snapshot, err := api.store.LoadTask(ctx, task.TaskID); err == nil {
		result.RuntimeStatus = string(snapshot.Status)
		result.Objective = snapshot.Objective
		result.Pipeline = snapshot.Pipeline
		result.LastItemSequence = snapshot.LastItemSequence
		result.LastEventSequence = snapshot.LastEventSequence
	}
	return result
}

func parseAfter(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	after, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("cursor must be a non-negative sequence number")
	}
	return after, nil
}

func terminalServerTask(status store.ServerTaskStatus) bool {
	return status == store.ServerTaskCompleted || status == store.ServerTaskFailed || status == store.ServerTaskCancelled
}

func decodeJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON request: %w", err)
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func requireEmptyBody(response http.ResponseWriter, request *http.Request) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBody)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		return errors.New("request body must be empty")
	}
	return nil
}

func writeMappedError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, erkaruntime.ErrNotFound):
		writeAPIError(response, http.StatusNotFound, "not_found", "Resource not found")
	case errors.Is(err, erkaruntime.ErrConflict):
		writeAPIError(response, http.StatusConflict, "conflict", "Resource state conflicts with this operation")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAPIError(response, http.StatusRequestTimeout, "request_cancelled", "Request was cancelled")
	default:
		writeAPIError(response, http.StatusInternalServerError, "internal_error", "The server could not complete the request")
	}
}

func writeAPIError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

type principalDTO struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type projectDTO struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	RepositoryRoot string    `json:"repositoryRoot"`
	DefaultRef     string    `json:"defaultRef"`
	CreatedAt      time.Time `json:"createdAt"`
}

func projectResponse(project store.ServerProject) projectDTO {
	return projectDTO{ID: project.ID, Name: project.Name, RepositoryRoot: project.RepositoryRoot, DefaultRef: project.DefaultRef, CreatedAt: project.CreatedAt}
}

type taskDTO struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	Objective         string    `json:"objective,omitempty"`
	Pipeline          string    `json:"pipeline,omitempty"`
	Status            string    `json:"status"`
	RuntimeStatus     string    `json:"runtimeStatus,omitempty"`
	LastItemSequence  uint64    `json:"lastItemSequence"`
	LastEventSequence uint64    `json:"lastEventSequence"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	// Outcome is accepted or rejected once the review is finalized; accepted
	// changes live on ResultBranch at ResultCommit.
	Outcome      string     `json:"outcome,omitempty"`
	ResultBranch string     `json:"resultBranch,omitempty"`
	ResultRef    string     `json:"resultRef,omitempty"`
	ResultCommit string     `json:"resultCommit,omitempty"`
	FinalizedAt  *time.Time `json:"finalizedAt,omitempty"`
}

type itemDTO struct {
	ID       string    `json:"id"`
	Sequence uint64    `json:"sequence"`
	TaskID   string    `json:"taskId"`
	RunID    string    `json:"runId"`
	Kind     string    `json:"kind"`
	Time     time.Time `json:"time"`
	Payload  any       `json:"payload"`
}

func itemResponse(item erkaruntime.RuntimeItem) itemDTO {
	var payload any
	switch item.Kind {
	case erkaruntime.ItemUserMessage, erkaruntime.ItemAssistantMessage, erkaruntime.ItemReasoningSummary:
		payload = map[string]any{"content": item.Message.Content}
	case erkaruntime.ItemToolCall:
		payload = map[string]any{"callId": item.ToolCall.CallID, "tool": item.ToolCall.Tool, "arguments": item.ToolCall.Arguments, "mutating": item.ToolCall.Mutating, "idempotencyKey": item.ToolCall.IdempotencyKey}
	case erkaruntime.ItemToolResult:
		payload = map[string]any{"callId": item.ToolResult.CallID, "output": item.ToolResult.Output, "isError": item.ToolResult.IsError, "truncated": item.ToolResult.Truncated}
	case erkaruntime.ItemUsage:
		payload = map[string]any{"inputTokens": item.Usage.InputTokens, "outputTokens": item.Usage.OutputTokens, "cacheReadTokens": item.Usage.CacheReadTokens, "cacheWriteTokens": item.Usage.CacheWriteTokens, "costMicrounits": item.Usage.CostMicrounits, "currency": item.Usage.Currency}
	case erkaruntime.ItemCheckpoint:
		payload = item.Checkpoint
	case erkaruntime.ItemError:
		payload = map[string]any{"code": item.Error.Code, "message": item.Error.Message, "retryable": item.Error.Retryable}
	}
	return itemDTO{ID: string(item.ID), Sequence: item.Sequence, TaskID: string(item.TaskID), RunID: string(item.RunID), Kind: string(item.Kind), Time: item.Time, Payload: payload}
}

type eventDTO struct {
	ID       string          `json:"id"`
	Sequence uint64          `json:"sequence"`
	TaskID   string          `json:"taskId"`
	RunID    string          `json:"runId,omitempty"`
	Type     string          `json:"type"`
	Time     time.Time       `json:"time"`
	Message  string          `json:"message,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func eventResponse(event erkaruntime.Event) eventDTO {
	return eventDTO{ID: string(event.ID), Sequence: event.Sequence, TaskID: string(event.TaskID), RunID: string(event.RunID), Type: event.Type, Time: event.Time, Message: event.Message, Data: event.Data}
}

type approvalDTO struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"taskId"`
	Kind        string          `json:"kind"`
	Message     string          `json:"message"`
	Status      string          `json:"status"`
	Actor       string          `json:"actor,omitempty"`
	RequestedAt time.Time       `json:"requestedAt"`
	ExpiresAt   *time.Time      `json:"expiresAt,omitempty"`
	DecidedAt   *time.Time      `json:"decidedAt,omitempty"`
	Tool        string          `json:"tool,omitempty"`
	Details     json.RawMessage `json:"details,omitempty"`
	Options     []string        `json:"options,omitempty"`
	Response    string          `json:"response,omitempty"`
	Scope       string          `json:"scope,omitempty"`
}

func approvalResponse(approval store.Approval) approvalDTO {
	result := approvalDTO{ID: approval.ID, TaskID: string(approval.TaskID), Kind: approval.Kind, Message: approval.Message, Status: string(approval.Status), Actor: approval.Actor, RequestedAt: approval.RequestedAt, Tool: approval.Tool, Details: approval.Details, Options: approval.Options, Response: approval.Response, Scope: string(approval.Scope)}
	if !approval.ExpiresAt.IsZero() {
		expires := approval.ExpiresAt
		result.ExpiresAt = &expires
	}
	if !approval.DecidedAt.IsZero() {
		decided := approval.DecidedAt
		result.DecidedAt = &decided
	}
	return result
}

var _ APIStore = (*store.SQLite)(nil)
