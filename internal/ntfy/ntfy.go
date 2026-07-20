package ntfy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
)

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
	publishClient   *http.Client
	subscribeClient *http.Client
	reconnectWait   time.Duration // pause between subscribe attempts; overridable so tests don't sleep for real
}

func New(cfg *config.Config) *Client {
	return &Client{
		cfg: cfg,
		publishClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		subscribeClient: &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Second},
		},
		reconnectWait: 5 * time.Second,
	}
}

func (c *Client) Send(ctx context.Context, r reminder.Reminder, late bool) error {
	topics := r.OutboundTopics
	if len(topics) == 0 {
		return fmt.Errorf("reminder %s has no outbound topics", r.ID)
	}

	for _, topic := range topics {
		mods := ntfyMessageModifications{
			title:    "Reminder",
			late:     late,
			tags:     r.Tags,
			priority: r.Priority,
			click:    r.Click,
		}
		if err := c.sendToTopic(ctx, r.Text, topic, mods); err != nil {
			return fmt.Errorf("failed to send to topic %s: %w", topic, err)
		}
	}
	return nil
}

func (c *Client) sendToTopic(ctx context.Context, text, topic string, mods ...ntfyMessageModifications) error {
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
		return err
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

	if mod.title != "" {
		req.Header.Set("Title", mod.title)
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

	slog.Info("notification sent", "topic", topic)
	return nil
}

func (c *Client) Run(ctx context.Context, create func(text string, outbound []string) (*reminder.Reminder, error)) {
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
		c.consume(ctx, msgs, create)
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

func (c *Client) consume(ctx context.Context, msgs <-chan subscriptionMessage, create func(text string, outbound []string) (*reminder.Reminder, error)) {
	for msg := range msgs {
		rem, err := create(msg.Text, msg.Outbound)
		if err != nil {
			slog.Error("failed to create reminder from ntfy", "err", err)
			continue
		}
		slog.Info("reminder created via ntfy", "topic", rem.OutboundTopics, "id", rem.ID, "due", rem.DueAt)

		if err := c.sendConfirmation(ctx, msg.Inbound, rem); err != nil {
			slog.Error("failed to send confirmation", "err", err)
		}
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

func (c *Client) sendSystem(ctx context.Context, text, topic string) error {
	return c.sendToTopic(ctx, "[later] "+text, topic)
}
