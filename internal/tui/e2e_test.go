package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/auth"
	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	keyring "github.com/zalando/go-keyring"
)

func TestHeadlessInteractiveWorkflowQueuesRestoresAndKeepsSecretOutOfState(t *testing.T) {
	ctx := context.Background()
	root, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root)
	ring := &memoryRing{values: make(map[string]string)}
	credentials := auth.NewManager(ring)
	if _, err := credentials.Save(ctx, "deepseek", "https://api.deepseek.com", "fixture-secret"); err != nil {
		t.Fatal(err)
	}
	secret, source, err := credentials.Resolve(ctx, "deepseek", "https://api.deepseek.com/", "")
	if err != nil || secret != "fixture-secret" || source != auth.SourceKeyring {
		t.Fatalf("credential = %q, %q, %v", secret, source, err)
	}
	patch := "diff --git a/answer.txt b/answer.txt\n--- a/answer.txt\n+++ b/answer.txt\n@@ -1 +1 @@\n-old\n+new\n"
	arguments, _ := json.Marshal(map[string]string{"patch": patch})
	provider := &gatedProvider{started: make(chan struct{}), release: make(chan struct{}), responses: [][]erkaruntime.ModelOutput{
		{{Kind: erkaruntime.ModelToolCallDelta, ToolCall: &erkaruntime.ToolCallDelta{CallID: "patch-1", Tool: "apply_patch", ArgumentsFragment: string(arguments)}}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "first complete"}, {Kind: erkaruntime.ModelCompleted}},
		{{Kind: erkaruntime.ModelTextDelta, Text: "follow-up complete"}, {Kind: erkaruntime.ModelCompleted}},
	}}
	ids := &e2eIDs{}
	live := make(chan erkaruntime.LiveUpdate, 64)
	session, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, Provider: provider, IDs: ids, Observer: erkaruntime.LiveObserverFunc(func(_ context.Context, update erkaruntime.LiveUpdate) { live <- update })})
	if err != nil {
		t.Fatal(err)
	}
	model := NewModel(Options{Context: ctx, Controller: session, Provider: "deepseek", Model: "fixture", Live: live})
	model.composer.InsertString("make the change")
	_, submit := model.Update(key("enter"))
	submitted := submit()
	_, run := model.Update(submitted)
	finished := make(chan any, 1)
	go func() { finished <- run() }()
	<-provider.started
	model.composer.InsertString("check the result")
	_, queue := model.Update(key("enter"))
	queued := queue()
	_, competing := model.Update(queued)
	if competing != nil {
		t.Fatal("queued follow-up started a competing run")
	}
	close(provider.release)
	firstFinished := (<-finished).(runFinishedMsg)
	_, load := model.Update(firstFinished)
	loaded := load()
	_, secondRun := model.Update(loaded)
	secondFinished := secondRun().(runFinishedMsg)
	_, reload := model.Update(secondFinished)
	model.Update(reload())
	_, diffCommand := model.Update(key("ctrl+d"))
	model.Update(diffCommand())
	if !strings.Contains(model.diff, "+new") {
		t.Fatalf("diff = %q", model.diff)
	}
	taskID := model.taskID
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := app.OpenSession(ctx, app.SessionOptions{WorkingDirectory: root, StateDir: state, IDs: &e2eIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	conversation, err := restored.OpenConversation(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Turns) != 2 || len(conversation.Runs) != 2 {
		t.Fatalf("restored conversation = %#v", conversation)
	}
	var users, assistants int
	for _, item := range conversation.Items {
		if item.Kind == erkaruntime.ItemUserMessage {
			users++
		}
		if item.Kind == erkaruntime.ItemAssistantMessage {
			assistants++
		}
	}
	if users != 2 || assistants != 2 || len(conversation.Events) == 0 {
		t.Fatalf("items/events = %d/%d/%d", users, assistants, len(conversation.Events))
	}
	contents, err := os.ReadFile(filepath.Join(root, "answer.txt"))
	if err != nil || strings.ReplaceAll(string(contents), "\r\n", "\n") != "new\n" {
		t.Fatalf("answer = %q, %v", contents, err)
	}
	database, err := os.ReadFile(filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(database), "fixture-secret") {
		t.Fatal("credential leaked into SQLite")
	}
}

type memoryRing struct{ values map[string]string }

func (ring *memoryRing) Get(service, user string) (string, error) {
	value, ok := ring.values[service+"/"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (ring *memoryRing) Set(service, user, password string) error {
	ring.values[service+"/"+user] = password
	return nil
}
func (ring *memoryRing) Delete(service, user string) error {
	delete(ring.values, service+"/"+user)
	return nil
}

type e2eIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *e2eIDs) NewID(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return fmt.Sprintf("%s_e2e_%03d", prefix, ids.next), nil
}

type gatedProvider struct {
	mu               sync.Mutex
	started, release chan struct{}
	once             sync.Once
	responses        [][]erkaruntime.ModelOutput
}

func (*gatedProvider) Name() string { return "fixture" }
func (*gatedProvider) Capabilities(context.Context) (erkaruntime.ModelCapabilities, error) {
	return erkaruntime.ModelCapabilities{Tools: true}, nil
}
func (provider *gatedProvider) Stream(ctx context.Context, request erkaruntime.ModelRequest) (erkaruntime.ModelStream, error) {
	provider.once.Do(func() {
		close(provider.started)
		select {
		case <-provider.release:
		case <-ctx.Done():
		}
	})
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.responses) == 0 {
		return nil, io.EOF
	}
	outputs := provider.responses[0]
	provider.responses = provider.responses[1:]
	return &e2eStream{outputs: outputs}, nil
}

type e2eStream struct{ outputs []erkaruntime.ModelOutput }

func (stream *e2eStream) Next(ctx context.Context) (erkaruntime.ModelOutput, error) {
	if err := ctx.Err(); err != nil {
		return erkaruntime.ModelOutput{}, err
	}
	if len(stream.outputs) == 0 {
		return erkaruntime.ModelOutput{}, io.EOF
	}
	value := stream.outputs[0]
	stream.outputs = stream.outputs[1:]
	return value, nil
}
func (*e2eStream) Close() error { return nil }

func gitFixture(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"add", "."}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
}

var _ auth.Keyring = (*memoryRing)(nil)
var _ erkaruntime.ModelProvider = (*gatedProvider)(nil)
