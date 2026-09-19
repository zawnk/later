package ntfy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	PreviewReminderText(text string) (task string, due time.Time, err error)
	ListStubs() ([]reminder.NamedStub, error)
	SetStub(name string, stub reminder.Stub) (created bool, err error)
	DeleteStub(name string) error
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

// buildActions mints a postpone token (shared by the "Snooze 1h"/"Tomorrow"
// buttons) and a separate clear-scoped token (for "Clear"), and returns the
// ntfy Actions header value for all three, or "" if base_url isn't
// configured (the feature just stays off). Three is ntfy's own max action
// count.
func (c *Client) buildActions(reminderID string) (string, error) {
	if c.cfg.Server.BaseURL == "" {
		return "", nil
	}

	base := strings.TrimRight(config.NormalizedBaseURL(c.cfg.Server.BaseURL), "/") + "/reminders/" + reminderID

	postponeToken, err := actiontoken.Mint(c.actionSecret, reminderID, "postpone")
	if err != nil {
		return "", err
	}

	labels := []string{"Snooze 1h", "Tomorrow"}
	durations := []string{"in 1h", "tomorrow morning"}
	actions := make([]string, 0, 3)
	for i, label := range labels {
		callback := base + "/postpone?duration=" + url.QueryEscape(durations[i])
		actions = append(actions, fmt.Sprintf("http, %s, %s, method=POST, headers.Authorization=Bearer %s, clear=true", label, callback, postponeToken))
	}

	clearToken, err := actiontoken.Mint(c.actionSecret, reminderID, "clear")
	if err != nil {
		return "", err
	}
	actions = append(actions, fmt.Sprintf("http, Clear, %s/dismiss, method=POST, headers.Authorization=Bearer %s, clear=true", base, clearToken))

	return strings.Join(actions, "; "), nil
}

// Clear marks each topic/id pair (from ArchivedReminder.NtfyMessageIDs) as
// read/dismissed on ntfy's server, which syncs across every subscribed
// client - not just the device that tapped the "Clear" button (that's
// what the button's own clear=true attribute already covers, locally
// only). Best-effort across topics: one topic failing doesn't stop the
// others from being attempted. Unlike Send (which stops at the first
// topic failure and relies on the scheduler's next-tick retry to catch
// the rest), there's no retry mechanism here - a one-off dismiss action
// only gets this one attempt, so it has to make its own best effort
// across every topic in a single pass rather than deferring to a retry
// that doesn't exist for this path. Only reports an error if every topic
// failed.
func (c *Client) Clear(ctx context.Context, topicIDs map[string]string) error {
	var failed []string
	for topic, id := range topicIDs {
		if err := c.clearTopic(ctx, topic, id); err != nil {
			slog.Error("failed to clear ntfy notification", "topic", topic, "id", id, "err", err)
			failed = append(failed, topic)
		}
	}

	if len(failed) > 0 && len(failed) == len(topicIDs) {
		return fmt.Errorf("failed to clear notification on every topic: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (c *Client) clearTopic(ctx context.Context, topic, id string) error {
	url := fmt.Sprintf("%s/%s/%s/clear", strings.TrimRight(c.cfg.Ntfy.Server, "/"), topic, id)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Ntfy.Token)

	resp, err := c.publishClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("ntfy returned status %d: %s", resp.StatusCode, body)
	}
	return nil
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

		slog.Error("ntfy subscription dropped, reconnecting", "backoff", c.reconnectWait.String(), "topics", combined, "err", err)

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
		if verb, rest, ok := cutCommand(msg.Text); ok {
			if handle, known := commands[verb]; known {
				handle(c, ctx, msg.Inbound, rest)
				continue
			}
			// An unrecognised slash word is not a command, it is text:
			// it falls through to reminder creation the way it always
			// has, so a message that merely starts with a slash still
			// becomes a reminder rather than an error about a verb.
		}

		text, tags, priority, err := parseDirectives(msg.Text)
		if err != nil {
			slog.Error("failed to parse inbound directives", "err", err)
			c.replyError(ctx, msg.Inbound, err)
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
			c.replyError(ctx, msg.Inbound, err)
			continue
		}
		slog.Info("reminder created via ntfy", "topic", rem.OutboundTopics, "id", rem.ID, "due", rem.DueAt)

		if err := c.sendConfirmation(ctx, msg.Inbound, rem); err != nil {
			slog.Error("failed to send confirmation", "err", err)
		}
	}
}

// commandHandler handles one inbound slash command: inbound is the
// topic the message arrived on, which is also the topic any reply goes
// back out on, and rest is everything after the verb.
type commandHandler func(c *Client, ctx context.Context, inbound, rest string)

// commands is the inbound command table, keyed by lowercased verb.
// Adding a command is adding a row here; nothing in consume changes.
//
// Defining verbs live in this slash namespace and invocation stays
// ":name", so the two never collide and no name needs reserving:
// "/stub rm ..." defines a stub named "rm" only if someone writes it,
// while deleting is its own verb.
var commands = map[string]commandHandler{
	"/test":   (*Client).handleTestParse,
	"/stub":   (*Client).handleStubSet,
	"/unstub": (*Client).handleStubDelete,
	"/stubs":  (*Client).handleStubList,
}

// cutCommand splits text into a lowercased leading slash verb and the
// rest of the message, trimmed. The verb is lowered because a phone
// keyboard autocapitalizes the first word of a message; the rest is
// left exactly as typed, since stub names and reminder text are the
// user's own bytes.
//
// It reports every "/word" opener as a command, known or not - which of
// them actually is one is the table's decision, not the splitter's.
func cutCommand(text string) (verb, rest string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	verb, rest, _ = strings.Cut(trimmed, " ")
	return strings.ToLower(verb), strings.TrimSpace(rest), true
}

// handleStubSet defines or replaces a stub: the first word of rest is
// the name, the remainder is the body. The body goes through
// parseDirectives, the same trailing "#tag !priority" grammar a plain
// reminder uses, so defining a stub needs no second syntax.
//
// The name is passed through exactly as typed. Only the verb folds
// case; a write names its stub explicitly, so "Hockey" is the service's
// error to report rather than something to silently fold here.
func (c *Client) handleStubSet(ctx context.Context, inbound, rest string) {
	name, body, _ := strings.Cut(rest, " ")
	name = trimStubSigil(name)
	body = strings.TrimSpace(body)
	if name == "" || body == "" {
		c.replyError(ctx, inbound, errors.New(`usage: /stub <name> <text> - e.g. "/stub hockey in 15m back to the game #hockey !high"`))
		return
	}

	text, tags, priority, err := parseDirectives(body)
	if err != nil {
		slog.Error("failed to parse inbound directives for /stub", "err", err)
		c.replyError(ctx, inbound, err)
		return
	}

	stub := reminder.Stub{Text: text, Tags: tags, Priority: priority}
	created, err := c.svc.SetStub(name, stub)
	if err != nil {
		slog.Error("failed to define stub from ntfy", "name", name, "err", err)
		c.replyError(ctx, inbound, err)
		return
	}
	slog.Info("stub defined via ntfy", "name", name, "created", created)

	verb := "updated"
	if created {
		verb = "created"
	}
	// No due time: the stub resolves fresh on every invocation, so a
	// moment computed now is one it will never fire at.
	if err := c.sendSystem(ctx, fmt.Sprintf("Stub :%s %s ✅ %s", name, verb, describeStub(stub)), inbound); err != nil {
		slog.Error("failed to send stub confirmation", "err", err)
	}
}

// handleStubDelete removes a stub. Deleting is its own verb rather than
// a sub-verb of /stub: "/stub rm hockey" could not be told apart from
// defining a stub named "rm", and an empty body meaning delete would
// make a truncated message destructive.
func (c *Client) handleStubDelete(ctx context.Context, inbound, rest string) {
	name := trimStubSigil(strings.TrimSpace(rest))
	if name == "" || strings.ContainsAny(name, " \t") {
		c.replyError(ctx, inbound, errors.New(`usage: /unstub <name> - e.g. "/unstub hockey"`))
		return
	}

	if err := c.svc.DeleteStub(name); err != nil {
		slog.Error("failed to delete stub from ntfy", "name", name, "err", err)
		c.replyError(ctx, inbound, err)
		return
	}
	slog.Info("stub deleted via ntfy", "name", name)

	if err := c.sendSystem(ctx, fmt.Sprintf("Stub :%s deleted ✅", name), inbound); err != nil {
		slog.Error("failed to send stub deletion confirmation", "err", err)
	}
}

// handleStubList replies with every defined stub, one per line, each
// named as it is invoked so the reply doubles as a list of what can be
// typed next.
func (c *Client) handleStubList(ctx context.Context, inbound, rest string) {
	stubs, err := c.svc.ListStubs()
	if err != nil {
		slog.Error("failed to list stubs from ntfy", "err", err)
		c.replyError(ctx, inbound, err)
		return
	}

	msg := "No stubs defined."
	if len(stubs) > 0 {
		lines := make([]string, 0, len(stubs)+1)
		lines = append(lines, fmt.Sprintf("%d stub(s):", len(stubs)))
		for _, stub := range stubs {
			lines = append(lines, fmt.Sprintf(":%s %s", stub.Name, describeStub(stub.Stub)))
		}
		msg = strings.Join(lines, "\n")
	}

	if err := c.sendSystem(ctx, msg, inbound); err != nil {
		slog.Error("failed to send stub list", "err", err)
	}
}

// describeStub renders what a stub expands to: the text quoted, so its
// boundaries are visible against the directives that follow, then those
// directives in the "#tag !priority" form they were defined with. It
// reads back as what was typed, but it is not itself retypeable - the
// quotes would become part of the text - and a listing names the stub
// ":name" for invoking, not for redefining. Click has no directive form
// and is shown plainly.
func describeStub(stub reminder.Stub) string {
	parts := []string{fmt.Sprintf("%q", stub.Text)}
	for _, tag := range stub.Tags {
		parts = append(parts, "#"+tag)
	}
	if stub.Priority != "" {
		parts = append(parts, "!"+stub.Priority)
	}
	if stub.Click != "" {
		parts = append(parts, stub.Click)
	}
	return strings.Join(parts, " ")
}

// trimStubSigil strips the invocation sigil from a name typed with it.
// Stubs are stored unprefixed, but "/stubs" lists them as ":name" so
// each line doubles as the invocation, and a name pasted back off that
// line has to work. `later stub set|del` is tolerant the same way.
func trimStubSigil(name string) string {
	return strings.TrimPrefix(name, ":")
}

// replyError reports err back on the topic the command arrived on,
// logging a failure to send rather than propagating it: a handler has
// nowhere left to return an error to.
func (c *Client) replyError(ctx context.Context, inbound string, err error) {
	if sendErr := c.sendError(ctx, inbound, err); sendErr != nil {
		slog.Error("failed to send error feedback", "err", sendErr)
	}
}

// handleTestParse previews rest the same way a real create would - through
// parseDirectives first, so a "/test buy milk tomorrow #work" preview
// never shows a tag stuck in the task text a real send would have
// stripped, then through PreviewReminderText, so "/test :hockey" shows
// what the stub would schedule rather than the invocation itself - then
// replies with task+due only (stripped tags/priority aren't echoed back;
// this is a preview of the *time* parsing).
func (c *Client) handleTestParse(ctx context.Context, inbound, rest string) {
	text, _, _, err := parseDirectives(rest)
	if err != nil {
		slog.Error("failed to parse inbound directives for /test", "err", err)
		c.replyError(ctx, inbound, err)
		return
	}

	task, due, err := c.svc.PreviewReminderText(text)
	if err != nil {
		slog.Error("failed to preview parse from ntfy", "err", err)
		c.replyError(ctx, inbound, err)
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
