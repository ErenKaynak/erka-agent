package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/app"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestAPIRequiresScopedBearerTokens(t *testing.T) {
	t.Parallel()
	fixture := newAPIFixture(t)

	response := fixture.request(t, http.MethodGet, "/v1/health", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", response.Code, response.Body.String())
	}
	response = fixture.request(t, http.MethodGet, "/v1/me", "", nil)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous me = %d, %q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
	projectBody := map[string]any{"name": "fixture", "repositoryRoot": fixture.source, "defaultRef": "HEAD"}
	response = fixture.request(t, http.MethodPost, "/v1/projects", fixture.readToken, projectBody)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("read token project creation = %d: %s", response.Code, response.Body.String())
	}
	response = fixture.request(t, http.MethodPost, "/v1/projects", fixture.adminToken, projectBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("admin project creation = %d: %s", response.Code, response.Body.String())
	}
	var project projectDTO
	if err := json.Unmarshal(response.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	response = fixture.request(t, http.MethodPatch, "/v1/projects/"+project.ID, fixture.adminToken, map[string]any{"name": "renamed"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"renamed"`) {
		t.Fatalf("project update = %d: %s", response.Code, response.Body.String())
	}
	response = fixture.request(t, http.MethodGet, "/v1/projects", fixture.readToken, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"renamed"`) {
		t.Fatalf("project listing = %d: %s", response.Code, response.Body.String())
	}
	response = fixture.request(t, http.MethodGet, "/v1/unknown", "", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous unknown route = %d: %s", response.Code, response.Body.String())
	}
}

func TestAPITaskEventsResumeAndApprovalDecision(t *testing.T) {
	t.Parallel()
	fixture := newAPIFixture(t)
	project := fixture.createProject(t)
	response := fixture.request(t, http.MethodPost, "/v1/tasks", fixture.adminToken, map[string]any{"projectId": project.ID, "objective": "inspect fixture"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("task creation = %d: %s", response.Code, response.Body.String())
	}
	var created taskDTO
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" || created.Status != string(store.ServerTaskQueued) {
		t.Fatalf("created task = %#v, %v", created, err)
	}
	worked, err := fixture.service.ExecuteNext(context.Background(), "api-worker")
	if err != nil || !worked {
		t.Fatalf("ExecuteNext() = %v, %v", worked, err)
	}

	response = fixture.request(t, http.MethodGet, "/v1/tasks/"+created.ID+"/events", fixture.readToken, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("event listing = %d: %s", response.Code, response.Body.String())
	}
	var listed struct {
		Events []eventDTO `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Events) < 2 {
		t.Fatalf("events = %#v, %v", listed.Events, err)
	}
	first := listed.Events[0]
	unauthorized := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+created.ID+"/events/stream", nil)
	unauthorized.Header.Set("Last-Event-ID", "unknown-event")
	unauthorizedResponse := httptest.NewRecorder()
	fixture.api.Handler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized resume = %d: %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+created.ID+"/events/stream", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.readToken)
	request.Header.Set("Last-Event-ID", first.ID)
	stream := httptest.NewRecorder()
	fixture.api.Handler().ServeHTTP(stream, request)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "id: "+listed.Events[1].ID) || strings.Contains(stream.Body.String(), "id: "+first.ID+"\n") {
		t.Fatalf("resumed stream = %d: %s", stream.Code, stream.Body.String())
	}

	response = fixture.request(t, http.MethodGet, "/v1/approvals?taskId="+created.ID+"&status=pending", fixture.readToken, nil)
	var approvals struct {
		Approvals []approvalDTO `json:"approvals"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &approvals); response.Code != http.StatusOK || err != nil || len(approvals.Approvals) != 1 {
		t.Fatalf("approvals = %d, %#v, %v: %s", response.Code, approvals, err, response.Body.String())
	}
	approvalID := approvals.Approvals[0].ID
	response = fixture.request(t, http.MethodPost, "/v1/approvals/"+approvalID+"/decision", fixture.readToken, map[string]any{"decision": "approved"})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("read token approval = %d: %s", response.Code, response.Body.String())
	}
	response = fixture.request(t, http.MethodPost, "/v1/approvals/"+approvalID+"/decision", fixture.adminToken, map[string]any{"decision": "approved"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"approved"`) {
		t.Fatalf("approval decision = %d: %s", response.Code, response.Body.String())
	}
}

func TestSSEDisconnectDoesNotCancelWorker(t *testing.T) {
	t.Parallel()
	provider := &blockingServerProvider{started: make(chan struct{}), release: make(chan struct{})}
	fixture := newAPIFixtureWithProvider(t, provider)
	project := fixture.createProject(t)
	response := fixture.request(t, http.MethodPost, "/v1/tasks", fixture.adminToken, map[string]any{"projectId": project.ID, "objective": "keep running"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("task creation = %d: %s", response.Code, response.Body.String())
	}
	var task taskDTO
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	workerResult := make(chan error, 1)
	go func() {
		_, executeErr := fixture.service.ExecuteNext(context.Background(), "disconnect-worker")
		workerResult <- executeErr
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		close(provider.release)
		t.Fatal("worker did not start")
	}
	streamCtx, disconnect := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/events/stream", nil).WithContext(streamCtx)
	request.Header.Set("Authorization", "Bearer "+fixture.readToken)
	stream := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	streamDone := make(chan struct{})
	go func() {
		fixture.api.Handler().ServeHTTP(stream, request)
		close(streamDone)
	}()
	select {
	case <-stream.flushed:
	case <-time.After(5 * time.Second):
		close(provider.release)
		t.Fatal("event stream did not flush headers")
	}
	disconnect()
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		close(provider.release)
		t.Fatal("event stream did not observe disconnect")
	}
	select {
	case err := <-workerResult:
		close(provider.release)
		t.Fatalf("worker stopped with disconnected client: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	running, err := fixture.database.LoadServerTask(context.Background(), erkaruntime.TaskID(task.ID))
	if err != nil || running.Status != store.ServerTaskRunning {
		close(provider.release)
		t.Fatalf("task after disconnect = %#v, %v", running, err)
	}
	close(provider.release)
	select {
	case err := <-workerResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish")
	}
}

type apiFixture struct {
	api        *API
	database   *store.SQLite
	service    *Service
	source     string
	readToken  string
	adminToken string
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func (recorder *flushRecorder) Flush() {
	recorder.ResponseRecorder.Flush()
	recorder.once.Do(func() { close(recorder.flushed) })
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	provider := &serverProvider{responses: [][]erkaruntime.ModelOutput{{
		{Kind: erkaruntime.ModelTextDelta, Text: "API task completed."},
		{Kind: erkaruntime.ModelCompleted},
	}}}
	return newAPIFixtureWithProvider(t, provider)
}

func newAPIFixtureWithProvider(t *testing.T, provider erkaruntime.ModelProvider) *apiFixture {
	t.Helper()
	ctx := context.Background()
	source := initSourceRepository(t)
	state := t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return openFixtureSession(ctx, state, root, provider)
	}
	service, err := NewService(ServiceOptions{StateDir: state, Store: database, RuntimeStore: database, OpenSession: openSession})
	if err != nil {
		t.Fatal(err)
	}
	readToken, adminToken := "read-secret", "admin-secret"
	now := time.Now().UTC()
	for _, token := range []store.APIToken{
		{ID: "read-token", Name: "reader", Hash: sha256.Sum256([]byte(readToken)), Scopes: []string{"read"}, CreatedAt: now},
		{ID: "admin-token", Name: "owner", Hash: sha256.Sum256([]byte(adminToken)), Scopes: []string{"admin"}, CreatedAt: now},
	} {
		if err := database.CreateAPIToken(ctx, token); err != nil {
			t.Fatal(err)
		}
	}
	api, err := NewAPI(APIOptions{Service: service, Store: database, SSEPoll: time.Millisecond, SSEHeartbeat: 10 * time.Millisecond, Pairings: database})
	if err != nil {
		t.Fatal(err)
	}
	return &apiFixture{api: api, database: database, service: service, source: source, readToken: readToken, adminToken: adminToken}
}

func openFixtureSession(ctx context.Context, state, root string, provider erkaruntime.ModelProvider) (TaskSession, error) {
	return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture"})
}

func (fixture *apiFixture) createProject(t *testing.T) projectDTO {
	t.Helper()
	response := fixture.request(t, http.MethodPost, "/v1/projects", fixture.adminToken, map[string]any{"name": "fixture", "repositoryRoot": fixture.source, "defaultRef": "HEAD"})
	if response.Code != http.StatusCreated {
		t.Fatalf("project creation = %d: %s", response.Code, response.Body.String())
	}
	var project projectDTO
	if err := json.Unmarshal(response.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	return project
}

func (fixture *apiFixture) request(t *testing.T, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	fixture.api.Handler().ServeHTTP(response, request)
	return response
}
