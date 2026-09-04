package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientAuthenticatesAndDecodesProjects(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://erka.example/v1/projects" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request = %s, %q", request.URL, request.Header.Get("Authorization"))
		}
		return response(http.StatusOK, `{"projects":[{"id":"project-1","name":"shop"}]}`), nil
	})
	remote, err := New("https://erka.example/", "secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	projects, err := remote.Projects(context.Background())
	if err != nil || len(projects) != 1 || projects[0].ID != "project-1" {
		t.Fatalf("projects = %#v, %v", projects, err)
	}
}

func TestClientRejectsInsecureRemoteAndDecodesAPIError(t *testing.T) {
	t.Parallel()
	if _, err := New("http://erka.example", "secret", nil); err == nil {
		t.Fatal("insecure remote URL accepted")
	}
	remote, err := New("http://127.0.0.1:7432", "secret", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusConflict, `{"error":{"code":"conflict","message":"wrong state"}}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	err = remote.Retry(context.Background(), "task-1")
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusConflict || apiErr.Code != "conflict" {
		t.Fatalf("API error = %#v", err)
	}
}

func TestClientResumesAndParsesSSE(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Last-Event-ID") != "event-1" || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("stream headers = %#v", request.Header)
		}
		body := ": heartbeat\n\nid: event-2\nevent: task.completed\ndata: {\"id\":\"event-2\",\"sequence\":2,\"taskId\":\"task-1\",\"type\":\"task.completed\"}\n\n"
		return response(http.StatusOK, body), nil
	})
	remote, err := New("https://erka.example", "secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	err = remote.Watch(context.Background(), "task-1", "event-1", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil || len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
