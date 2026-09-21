package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const (
	defaultNotificationPoll    = 2 * time.Second
	defaultNotificationHorizon = 7 * 24 * time.Hour
	maxNotificationBackoff     = 30 * time.Minute
	notificationBatch          = 50
)

type NotificationStore interface {
	PendingNotifications(context.Context, []string, time.Time, time.Duration, int) ([]store.NotificationDelivery, error)
	MarkNotificationDelivered(context.Context, string, string, time.Time) error
	MarkNotificationFailed(context.Context, string, string, time.Time, string) error
}

// NotificationTarget delivers one notification. Delivery must be safe to
// repeat: the dispatcher is at-least-once and targets receive a stable ID.
type NotificationTarget interface {
	Name() string
	Accepts(kind string) bool
	Deliver(context.Context, store.Notification) error
}

// Dispatcher drains the durable notification outbox. Delivery failures never
// affect task state; they are retried with exponential backoff.
type Dispatcher struct {
	store   NotificationStore
	targets []NotificationTarget
	clock   func() time.Time
	poll    time.Duration
	horizon time.Duration
}

func NewDispatcher(notificationStore NotificationStore, targets []NotificationTarget, clock func() time.Time) (*Dispatcher, error) {
	if notificationStore == nil {
		return nil, errors.New("notification dispatcher requires a store")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	names := make(map[string]bool)
	for _, target := range targets {
		if target == nil || target.Name() == "" || names[target.Name()] {
			return nil, errors.New("notification targets require unique names")
		}
		names[target.Name()] = true
	}
	return &Dispatcher{store: notificationStore, targets: targets, clock: clock, poll: defaultNotificationPoll, horizon: defaultNotificationHorizon}, nil
}

func (dispatcher *Dispatcher) Run(ctx context.Context) error {
	if len(dispatcher.targets) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	ticker := time.NewTicker(dispatcher.poll)
	defer ticker.Stop()
	for {
		if _, err := dispatcher.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// DispatchOnce attempts every due delivery once and reports how many succeeded.
func (dispatcher *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	byName := make(map[string]NotificationTarget, len(dispatcher.targets))
	names := make([]string, 0, len(dispatcher.targets))
	for _, target := range dispatcher.targets {
		byName[target.Name()] = target
		names = append(names, target.Name())
	}
	pending, err := dispatcher.store.PendingNotifications(ctx, names, dispatcher.clock().UTC(), dispatcher.horizon, notificationBatch)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, delivery := range pending {
		target := byName[delivery.Target]
		if target == nil {
			continue
		}
		if !target.Accepts(delivery.Notification.Kind) {
			if err := dispatcher.store.MarkNotificationDelivered(ctx, delivery.Notification.ID, delivery.Target, dispatcher.clock().UTC()); err != nil {
				return delivered, err
			}
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		deliverErr := target.Deliver(attemptCtx, delivery.Notification)
		cancel()
		now := dispatcher.clock().UTC()
		if deliverErr != nil {
			if ctx.Err() != nil {
				return delivered, ctx.Err()
			}
			if err := dispatcher.store.MarkNotificationFailed(ctx, delivery.Notification.ID, delivery.Target, now.Add(backoff(delivery.Attempts)), deliverErr.Error()); err != nil {
				return delivered, err
			}
			continue
		}
		if err := dispatcher.store.MarkNotificationDelivered(ctx, delivery.Notification.ID, delivery.Target, now); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

func backoff(attempts int) time.Duration {
	delay := 5 * time.Second
	for index := 0; index < attempts && delay < maxNotificationBackoff; index++ {
		delay *= 2
	}
	if delay > maxNotificationBackoff {
		delay = maxNotificationBackoff
	}
	return delay
}

// NotificationTargets builds configured targets, resolving secrets from the
// environment. A missing secret is a startup error rather than a silent skip.
func NotificationTargets(server config.Server, lookupEnv func(string) (string, bool), client *http.Client) ([]NotificationTarget, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	secret := func(name string) (string, error) {
		if name == "" {
			return "", nil
		}
		value, ok := lookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("notification secret environment %q is not set", name)
		}
		return strings.TrimSpace(value), nil
	}
	targets := make([]NotificationTarget, 0, len(server.Notifications))
	for _, configured := range server.Notifications {
		token, err := secret(configured.TokenEnv)
		if err != nil {
			return nil, err
		}
		base := baseTarget{name: configured.Name, url: configured.URL, token: token, events: configured.Events, client: client, publicURL: server.PublicURL}
		switch configured.Type {
		case config.NotificationNtfy:
			actionToken, err := secret(configured.ActionTokenEnv)
			if err != nil {
				return nil, err
			}
			targets = append(targets, &NtfyTarget{baseTarget: base, actionToken: actionToken})
		case config.NotificationWebhook:
			signing, err := secret(configured.SecretEnv)
			if err != nil {
				return nil, err
			}
			targets = append(targets, &WebhookTarget{baseTarget: base, secret: signing})
		default:
			return nil, fmt.Errorf("unsupported notification target type %q", configured.Type)
		}
	}
	return targets, nil
}

type baseTarget struct {
	name      string
	url       string
	token     string
	events    []string
	client    *http.Client
	publicURL string
}

func (target *baseTarget) Name() string { return target.name }

func (target *baseTarget) Accepts(kind string) bool {
	if len(target.events) == 0 {
		return true
	}
	for _, event := range target.events {
		if event == kind {
			return true
		}
	}
	return false
}

func (target *baseTarget) taskLink(notification store.Notification) string {
	if target.publicURL == "" {
		return ""
	}
	return target.publicURL + "/app/#/tasks/" + url.PathEscape(string(notification.TaskID))
}

func (target *baseTarget) post(ctx context.Context, endpoint string, body []byte, headers map[string]string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "erka-server")
	if target.token != "" {
		request.Header.Set("Authorization", "Bearer "+target.token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := target.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("%s responded %s", target.name, response.Status)
	}
	return nil
}

// WebhookTarget posts a stable JSON document. When a secret is configured the
// body is signed with HMAC-SHA256 in X-Erka-Signature.
type WebhookTarget struct {
	baseTarget
	secret string
}

type webhookPayload struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	TaskID     string    `json:"taskId"`
	ApprovalID string    `json:"approvalId,omitempty"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	Options    []string  `json:"options,omitempty"`
	Objective  string    `json:"objective,omitempty"`
	Link       string    `json:"link,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (target *WebhookTarget) Deliver(ctx context.Context, notification store.Notification) error {
	body, err := json.Marshal(webhookPayload{
		ID: notification.ID, Kind: notification.Kind, TaskID: string(notification.TaskID), ApprovalID: notification.ApprovalID,
		Title: notification.Title, Message: notification.Message, Options: notification.Options, Objective: notification.Objective,
		Link: target.taskLink(notification), CreatedAt: notification.CreatedAt,
	})
	if err != nil {
		return err
	}
	headers := map[string]string{"X-Erka-Event-Id": notification.ID}
	if target.secret != "" {
		mac := hmac.New(sha256.New, []byte(target.secret))
		mac.Write(body)
		headers["X-Erka-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return target.post(ctx, target.url, body, headers)
}

// NtfyTarget publishes through ntfy's JSON API. With an action token it adds
// approve/reject buttons that call the erka API directly from the phone.
type NtfyTarget struct {
	baseTarget
	actionToken string
}

type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title"`
	Message  string       `json:"message"`
	Tags     []string     `json:"tags,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Click    string       `json:"click,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

func (target *NtfyTarget) Deliver(ctx context.Context, notification store.Notification) error {
	parsed, err := url.Parse(target.url)
	if err != nil {
		return err
	}
	topic := path.Base(parsed.Path)
	if topic == "" || topic == "/" || topic == "." {
		return fmt.Errorf("ntfy target %q URL must end with a topic", target.name)
	}
	parsed.Path = path.Dir(parsed.Path)
	if parsed.Path == "." {
		parsed.Path = "/"
	}
	message := ntfyMessage{
		Topic: topic, Title: "erka: " + notification.Title, Message: ntfyBody(notification),
		Tags: ntfyTags(notification.Kind), Priority: ntfyPriority(notification.Kind), Click: target.taskLink(notification),
		Actions: target.actions(notification),
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return target.post(ctx, parsed.String(), body, map[string]string{"X-Erka-Event-Id": notification.ID})
}

func ntfyBody(notification store.Notification) string {
	body := notification.Message
	if objective := strings.TrimSpace(notification.Objective); objective != "" {
		if len(objective) > 160 {
			objective = strings.ToValidUTF8(objective[:160], "") + "…"
		}
		body += "\n\nTask: " + objective
	}
	return body
}

func ntfyTags(kind string) []string {
	switch kind {
	case "question":
		return []string{"question"}
	case "permission":
		return []string{"warning"}
	case "accept_changes":
		return []string{"white_check_mark"}
	case "task.failed":
		return []string{"x"}
	default:
		return []string{"bell"}
	}
}

func ntfyPriority(kind string) int {
	switch kind {
	case "question", "permission":
		return 4
	case "task.failed", "task.waiting":
		return 4
	default:
		return 3
	}
}

func (target *NtfyTarget) actions(notification store.Notification) []ntfyAction {
	if target.actionToken == "" || target.publicURL == "" || notification.ApprovalID == "" {
		return nil
	}
	decide := func(label, decision, response, scope string) ntfyAction {
		payload := map[string]string{"decision": decision}
		if response != "" {
			payload["response"] = response
		}
		if scope != "" {
			payload["scope"] = scope
		}
		body, _ := json.Marshal(payload)
		return ntfyAction{
			Action: "http", Label: label, Method: http.MethodPost, Clear: true,
			URL:     target.publicURL + "/v1/approvals/" + url.PathEscape(notification.ApprovalID) + "/decision",
			Headers: map[string]string{"Authorization": "Bearer " + target.actionToken, "Content-Type": "application/json"},
			Body:    string(body),
		}
	}
	switch notification.Kind {
	case "permission":
		return []ntfyAction{decide("Allow", "approved", "", ""), decide("Allow for task", "approved", "", "task"), decide("Deny", "rejected", "", "")}
	case "accept_changes":
		return []ntfyAction{decide("Accept", "approved", "", ""), decide("Reject", "rejected", "", "")}
	case "question":
		actions := make([]ntfyAction, 0, 3)
		for _, option := range notification.Options {
			if len(actions) == 3 {
				break
			}
			actions = append(actions, decide(option, "approved", option, ""))
		}
		return actions
	default:
		return nil
	}
}
