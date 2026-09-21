package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestServerQuestionParksNotifiesAndResumesAfterAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initSourceRepository(t)
	state := t.TempDir()
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	provider := &serverProvider{responses: [][]erkaruntime.ModelOutput{
		{
			{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: "call-ask", Tool: erkaruntime.AskUserToolName, ArgumentsFragment: `{"question":"Which database?","options":["sqlite","postgres"]}`}},
			{Kind: erkaruntime.ModelCompleted},
		},
		{{Kind: erkaruntime.ModelTextDelta, Text: "Using sqlite."}, {Kind: erkaruntime.ModelCompleted}},
	}}
	interactor, err := NewInteractor(database, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	openSession := func(ctx context.Context, root string) (TaskSession, error) {
		return app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, NativeModel: "fixture", Interactor: interactor})
	}
	service, err := NewService(ServiceOptions{StateDir: state, Store: database, RuntimeStore: database, OpenSession: openSession})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.RegisterProject(ctx, "fixture", source, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.SubmitTask(ctx, project.ID, "choose storage")
	if err != nil {
		t.Fatal(err)
	}

	if worked, err := service.ExecuteNext(ctx, "worker-1"); err != nil || !worked {
		t.Fatalf("first ExecuteNext() = %v, %v", worked, err)
	}
	stored, _ := database.LoadServerTask(ctx, task.TaskID)
	if stored.Status != store.ServerTaskWaiting {
		t.Fatalf("server task status = %q, want waiting", stored.Status)
	}
	pending, err := database.ListApprovals(ctx, task.TaskID, store.ApprovalPending)
	if err != nil || len(pending) != 1 || pending[0].Kind != "question" || pending[0].Message != "Which database?" || strings.Join(pending[0].Options, ",") != "sqlite,postgres" {
		t.Fatalf("pending approvals = %#v, %v", pending, err)
	}

	received := &ntfyRecorder{}
	endpoint := httptest.NewServer(received)
	t.Cleanup(endpoint.Close)
	targets, err := NotificationTargets(config.Server{PublicURL: "http://100.64.0.1:7432", Notifications: []config.NotificationTarget{{Name: "phone", Type: config.NotificationNtfy, URL: endpoint.URL + "/erka", ActionTokenEnv: "ACTION"}}}, func(name string) (string, bool) { return "approve-token", name == "ACTION" }, endpoint.Client())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(database, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if delivered, err := dispatcher.DispatchOnce(ctx); err != nil || delivered != 1 {
		t.Fatalf("DispatchOnce() = %d, %v", delivered, err)
	}
	if delivered, err := dispatcher.DispatchOnce(ctx); err != nil || delivered != 0 {
		t.Fatalf("second DispatchOnce() redelivered = %d, %v", delivered, err)
	}
	message := received.Messages()[0]
	if message.Topic != "erka" || message.Title != "erka: Question from erka" || len(message.Actions) != 2 || message.Actions[0].Label != "sqlite" || !strings.Contains(message.Actions[0].Body, `"response":"sqlite"`) {
		t.Fatalf("ntfy message = %#v", message)
	}

	if _, err := service.DecideApproval(ctx, store.ApprovalDecision{ID: pending[0].ID, Actor: "phone", Status: store.ApprovalApproved, Response: "sqlite", Time: pending[0].RequestedAt.Add(1)}); err != nil {
		t.Fatal(err)
	}
	stored, _ = database.LoadServerTask(ctx, task.TaskID)
	if stored.Status != store.ServerTaskQueued {
		t.Fatalf("server task after answer = %q, want queued", stored.Status)
	}
	if worked, err := service.ExecuteNext(ctx, "worker-1"); err != nil || !worked {
		t.Fatalf("resumed ExecuteNext() = %v, %v", worked, err)
	}
	stored, _ = database.LoadServerTask(ctx, task.TaskID)
	if stored.Status != store.ServerTaskCompleted {
		t.Fatalf("server task after resume = %q, want completed", stored.Status)
	}
	items, err := database.LoadItems(ctx, task.TaskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for _, item := range items {
		if item.Kind == erkaruntime.ItemToolResult && item.ToolResult.CallID == "call-ask" && item.ToolResult.Output == "User answer: sqlite" {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("answer was not recorded as the tool result: %#v", items)
	}
	if delivered, err := dispatcher.DispatchOnce(ctx); err != nil || delivered != 1 || received.Messages()[1].Title != "erka: Task completed" {
		t.Fatalf("completion notification = %d, %v, %#v", delivered, err, received.Messages())
	}
}

func TestDispatcherRetriesFailedDeliveryLater(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	seedServerTask(t, database)
	now := fixedNow()
	if err := database.CreateApproval(ctx, store.Approval{ID: "approval-1", TaskID: "task-1", Kind: "permission", Message: "Run command: make", Status: store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(time.Hour), Tool: "run_command"}); err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	failing.Store(true)
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			response.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(endpoint.Close)
	targets, err := NotificationTargets(config.Server{Notifications: []config.NotificationTarget{{Name: "hook", Type: config.NotificationWebhook, URL: endpoint.URL}}}, func(string) (string, bool) { return "", false }, endpoint.Client())
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	dispatcher, err := NewDispatcher(database, targets, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if delivered, err := dispatcher.DispatchOnce(ctx); err != nil || delivered != 0 {
		t.Fatalf("failing DispatchOnce() = %d, %v", delivered, err)
	}
	failing.Store(false)
	if delivered, _ := dispatcher.DispatchOnce(ctx); delivered != 0 {
		t.Fatal("delivery retried before its backoff elapsed")
	}
	clock = now.Add(time.Minute)
	if delivered, err := dispatcher.DispatchOnce(ctx); err != nil || delivered != 1 {
		t.Fatalf("retried DispatchOnce() = %d, %v", delivered, err)
	}
}

type ntfyRecorder struct {
	mu       sync.Mutex
	messages []ntfyMessage
}

func (recorder *ntfyRecorder) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	var message ntfyMessage
	if err := json.Unmarshal(body, &message); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	recorder.mu.Lock()
	recorder.messages = append(recorder.messages, message)
	recorder.mu.Unlock()
}

func (recorder *ntfyRecorder) Messages() []ntfyMessage {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]ntfyMessage(nil), recorder.messages...)
}

func fixedNow() time.Time { return time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC) }

func seedServerTask(t *testing.T, database *store.SQLite) {
	t.Helper()
	ctx, now := context.Background(), fixedNow()
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := database.CreateServerProject(ctx, store.ServerProject{ID: "server-project-1", Name: "fixture", RepositoryRoot: filepath.Join(t.TempDir(), "source"), DefaultRef: "main", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateProject(ctx, erkaruntime.Project{ID: "runtime-project-1", Root: worktree, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateTask(ctx, erkaruntime.Task{ID: "task-1", ProjectID: "runtime-project-1", Objective: "fix", Status: erkaruntime.TaskQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateServerTask(ctx, store.ServerTask{TaskID: "task-1", ServerProjectID: "server-project-1", WorktreeRoot: worktree, Status: store.ServerTaskQueued, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictNetworksRejectsPeersOutsideTrustedRanges(t *testing.T) {
	t.Parallel()
	networks, err := ParseTrustedNetworks([]string{"10.8.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	handler := RestrictNetworks(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }), networks)
	for remote, want := range map[string]int{"10.8.0.7:5000": http.StatusNoContent, "127.0.0.1:5000": http.StatusNoContent, "203.0.113.9:5000": http.StatusForbidden} {
		request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		request.RemoteAddr = remote
		request.Header.Set("X-Forwarded-For", "10.8.0.7")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("peer %s status = %d, want %d", remote, recorder.Code, want)
		}
	}
}
