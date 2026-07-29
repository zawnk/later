package ntfy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mergestat/timediff"

	"github.com/zawnk/later/internal/actiontoken"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/service"
)

// ageLineThreshold is the minimum time between a reminder's creation and
// its firing before the notification gets a "(set X ago)" second line.
// Below it, the reminder was basically immediate and the age adds no
// information.
const ageLineThreshold = time.Hour

// ReminderService is what Client needs from internal/service to handle
// inbound ntfy messages.
type ReminderService interface {
	CreateReminder(service.CreateInput) (*reminder.Reminder, error)
	ParseReminderText(text string) (task string, due time.Time, err error)
}

type subscriptionMessage struct {
	Text     string
	Outbound []string
	Inbound  string
}

type ntfyMessage struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Event   string `json:"event"`
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

type ntfyMessageModifications struct {
	title    string
	late     bool
	tags     []string
	priority string
	click    string
	actions  string
}

func priorityRank(p string) int {
	switch p {
	case "min", "1":
		return 1
	case "low", "2":
		return 2
	case "high", "4":
		return 4
	case "urgent", "max", "5":
		return 5
	default:
		return 3
	}
}

type Client struct {
	cfg             *config.Config
	actionSecret    []byte
	svc             ReminderService
	publishClient   *http.Client
	subscribeClient *http.Client
	reconnectWait   time.Duration
}

func New(cfg *config.Config, actionSecret []byte, svc ReminderService) *Client {
	return &Client{
		cfg:          cfg,
		actionSecret: actionSecret,
		svc:          svc,
		publishClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		subscribeClient: &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Second},
		},
		reconnectWait: 5 * time.Second,
	}
}

func (c *Client) Send(ctx context.Context, r reminder.Reminder, late bool) (map[string]string, error) {
	topics := r.OutboundTopics
	if len(topics) == 0 {
		return nil, fmt.Errorf("reminder %s has no outbound topics", r.ID)
	}

	text := r.Text
	if time.Since(r.CreatedAt) >= ageLineThreshold {
		text += fmt.Sprintf("\n(set %s)", timediff.TimeDiff(r.CreatedAt))
	}

	actions, err := c.buildActions(r.ID)
	if err != nil {
		slog.Error("failed to build action buttons, sending notification without them", "id", r.ID, "err", err)
	}

	ids := make(map[string]string, len(topics))
	for _, topic := range topics {
		mods := ntfyMessageModifications{
			title:    "Reminder",
			late:     late,
			tags:     r.Tags,
			priority: r.Priority,
			click:    r.Click,
			actions:  actions,
		}
		id, err := c.sendToTopic(ctx, text, topic, mods)
		if err != nil {
			return nil, fmt.Errorf("failed to send to topic %s: %w", topic, err)
		}
		ids[topic] = id
	}
	return ids, nil
}

// buildActions mints one postpone token for reminderID (the token only
// grants "postpone this reminder" and returns the ntfy Actions header
// value for the "Snooze 1h"/"Tomorrow" buttons, or "" if base_url isn't
// configured (the feature just stays off).
func (c *Client) buildActions(reminderID string) (string, error) {
	if c.cfg.Server.BaseURL == "" {
		return "", nil
	}

	token, err := actiontoken.Mint(c.actionSecret, reminderID, "postpone")
	if err != nil {
		return "", err
	}

	base := strings.TrimRight(config.NormalizedBaseURL(c.cfg.Server.BaseURL), "/") + "/reminders/" + reminderID + "/postpone"

	labels := []string{"Snooze 1h", "Tomorrow"}
	durations := []string{"in 1h", "tomorrow morning"}
	actions := make([]string, len(labels))
	for i, label := range labels {
		callback := base + "?duration=" + url.QueryEscape(durations[i])
		actions[i] = fmt.Sprintf("http, %s, %s, method=POST, headers.Authorization=Bearer %s, clear=true", label, callback, token)
	}

	return strings.Join(actions, "; "), nil
}

func (c *Client) sendToTopic(ctx context.Context, text, topic string, mods ...ntfyMessageModifications) (string, error) {
	var mod ntfyMessageModifications
	if len(mods) > 0 {
		mod = mods[0]
	}

	if mod.late {
		text = fmt.Sprintf("%s %s", c.cfg.LatePrefix, text)
	}

	url := fmt.Sprintf("%s/%s", strings.TrimRight(c.cfg.Ntfy.Server, "/"), topic)

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(text))
	if err != nil {
		return "", err
	}

	tags := mod.tags
	if mod.late && !slices.Contains(tags, "warning") {
		tags = append([]string{"warning"}, tags...)
	}
	if len(tags) > 0 {
		req.Header.Set("Tags", strings.Join(tags, ","))
	}

	priority := mod.priority
	if mod.late && priorityRank(priority) < priorityRank("high") {
		priority = "high"
	}
	if priority != "" {
		req.Header.Set("Priority", priority)
	}

	if mod.click != "" {
		req.Header.Set("Click", mod.click)
	}

	if mod.actions != "" {
		req.Header.Set("Actions", mod.actions)
	}

	if mod.title != "" {
		req.Header.Set("Title", mod.title)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Ntfy.Token)

	resp, err := c.publishClient.Do(req)
	if err != nil {
		return "", err
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("ntfy returned status %d: %s", resp.StatusCode, body)
	}

	var published struct {
		ID string `json:"id"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("failed to read ntfy publish response for topic %s: %w", topic, err)
	}
	if err := json.Unmarshal(body, &published); err != nil || published.ID == "" {
		return "", fmt.Errorf("ntfy publish to topic %s returned no message id (body: %s)", topic, body)
	}

	slog.Info("notification sent", "topic", topic, "id", published.ID)
	return published.ID, nil
}

func (c *Client) Run(ctx context.Context) {
	if len(c.cfg.Inbound) == 0 {
		slog.Info("no inbound topics configured, ntfy subscriber disabled")
		<-ctx.Done()
		slog.Info("ntfy subscriber stopped")
		return
	}

	msgs := make(chan subscriptionMessage, 32)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.consume(ctx, msgs)
	}()

	defer func() {
		close(msgs)
		wg.Wait()
	}()

	topics := make([]string, len(c.cfg.Inbound))
	for i, inbound := range c.cfg.Inbound {
		topics[i] = inbound.Topic
	}
	combined := strings.Join(topics, ",")
	var since string

	for {
		slog.Info("subscribing to ntfy topics", "topics", combined, "since", since)
		newSince, err := c.subscribe(ctx, combined, since, msgs)
		since = newSince

		if ctx.Err() != nil {
			slog.Info("shutdown signal received- ntfy subscriber stopped")
			return
		}

		slog.Error("ntfy subscription dropped, reconnecting", "backoff", c.reconnectWait, "topics", combined, "err", err)

		select {
		case <-ctx.Done():
			slog.Info("ntfy subscriber stopped during reconnect wait")
			return
		case <-time.After(c.reconnectWait):
		}
	}
}

func (c *Client) consume(ctx context.Context, msgs <-chan subscriptionMessage) {
	for msg := range msgs {
		if rest, ok := cutTestPrefix(msg.Text); ok {
			c.handleTestParse(ctx, msg.Inbound, rest)
			continue
		}

		text, tags, priority, err := parseDirectives(msg.Text)
		if err != nil {
			slog.Error("failed to parse inbound directives", "err", err)
			if sendErr := c.sendError(ctx, msg.Inbound, err); sendErr != nil {
				slog.Error("failed to send error feedback", "err", sendErr)
			}
			continue
		}

		rem, err := c.svc.CreateReminder(service.CreateInput{
			Text:           text,
			OutboundTopics: msg.Outbound,
			Tags:           tags,
			Priority:       priority,
		})
		if err != nil {
			slog.Error("failed to create reminder from ntfy", "err", err)
			if sendErr := c.sendError(ctx, msg.Inbound, err); sendErr != nil {
				slog.Error("failed to send error feedback", "err", sendErr)
			}
			continue
		}
		slog.Info("reminder created via ntfy", "topic", rem.OutboundTopics, "id", rem.ID, "due", rem.DueAt)

		if err := c.sendConfirmation(ctx, msg.Inbound, rem); err != nil {
			slog.Error("failed to send confirmation", "err", err)
		}
	}
}

// cutTestPrefix reports whether text is the "/test" diagnostic trigger
// (case-insensitive, e.g. from a phone keyboard's autocapitalize) and
// returns whatever follows it, trimmed. Matches a bare "/test" (rest "")
// and "/test <anything>" alike.
func cutTestPrefix(text string) (rest string, ok bool) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	if lower != "/test" && !strings.HasPrefix(lower, "/test ") {
		return "", false
	}
	return strings.TrimSpace(trimmed[len("/test"):]), true
}

// handleTestParse previews rest the same way a real create would - through
// parseDirectives first, so a "/test buy milk tomorrow #work" preview
// never shows a tag stuck in the task text a real send would have
// stripped - then replies with task+due only (stripped tags/priority
// aren't echoed back; this is a preview of the *time* parsing).
func (c *Client) handleTestParse(ctx context.Context, inbound, rest string) {
	text, _, _, err := parseDirectives(rest)
	if err != nil {
		slog.Error("failed to parse inbound directives for /test", "err", err)
		if sendErr := c.sendError(ctx, inbound, err); sendErr != nil {
			slog.Error("failed to send error feedback", "err", sendErr)
		}
		return
	}

	task, due, err := c.svc.ParseReminderText(text)
	if err != nil {
		slog.Error("failed to preview parse from ntfy", "err", err)
		if sendErr := c.sendError(ctx, inbound, err); sendErr != nil {
			slog.Error("failed to send error feedback", "err", sendErr)
		}
		return
	}

	if err := c.sendParsePreview(ctx, inbound, task, due); err != nil {
		slog.Error("failed to send parse preview", "err", err)
	}
}

func (c *Client) subscribe(ctx context.Context, topics, since string, incomingMsgs chan<- subscriptionMessage) (string, error) {
	url := fmt.Sprintf("%s/%s/json", strings.TrimRight(c.cfg.Ntfy.Server, "/"), topics)
	if since != "" {
		url += "?since=" + since
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return since, err
	}

	req.Header.Set("Authorization", "Bearer "+c.cfg.Ntfy.Token)

	resp, err := c.subscribeClient.Do(req)
	if err != nil {
		return since, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return since, fmt.Errorf("ntfy subscription failed with status %d: %s", resp.StatusCode, body)
	}

	slog.Info("successfully subscribed to ntfy topics", "host", c.cfg.Ntfy.Server, "topics", topics, "since", since)

	lastID := since

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg ntfyMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			slog.Warn("failed to parse ntfy message", "err", err)
			continue
		}

		if msg.ID != "" {
			lastID = msg.ID
		}

		if msg.Event != "message" {
			continue
		}

		if strings.HasPrefix(msg.Message, "[later]") {
			continue
		}

		outbound := c.resolveOutbound(msg.Topic)
		if len(outbound) == 0 {
			slog.Warn("dropping message on unexpected topic", "topic", msg.Topic)
			continue
		}
		sub := subscriptionMessage{Text: msg.Message, Outbound: outbound, Inbound: msg.Topic}

		select {
		case incomingMsgs <- sub:
		case <-ctx.Done():
			return lastID, ctx.Err()
		}
	}

	return lastID, scanner.Err()
}

func (c *Client) resolveOutbound(topic string) []string {
	for _, inbound := range c.cfg.Inbound {
		if inbound.Topic == topic {
			return slices.Clone(inbound.Outbound)
		}
	}
	return nil
}

func (c *Client) sendConfirmation(ctx context.Context, topic string, r *reminder.Reminder) error {
	msg := fmt.Sprintf("Reminder set for %s ✅ (%s)", r.DueAt.Format("Mon Jan 2, 15:04"), r.ID)
	return c.sendSystem(ctx, msg, topic)
}

func (c *Client) sendParsePreview(ctx context.Context, topic, task string, due time.Time) error {
	msg := fmt.Sprintf("%q -> %s", task, due.Format("Mon Jan 2, 15:04"))
	return c.sendSystem(ctx, msg, topic)
}

func (c *Client) sendError(ctx context.Context, topic string, createErr error) error {
	msg := fmt.Sprintf("error: %s", createErr)
	return c.sendSystem(ctx, msg, topic)
}

func (c *Client) sendSystem(ctx context.Context, text, topic string) error {
	_, err := c.sendToTopic(ctx, "[later] "+text, topic)
	return err
}
