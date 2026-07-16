package ntfy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
)

type SubscriptionMessage struct {
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
	title string
	late  bool
	tags  []string
}

type NtfyClient struct {
	cfg *config.Config
}

func NewNtfyClient(cfg *config.Config) *NtfyClient {
	return &NtfyClient{cfg: cfg}
}

func (c *NtfyClient) Send(r reminder.Reminder, late bool) error {
	topics := r.OutboundTopics
	if len(topics) == 0 {
		topics = []string{c.cfg.Ntfy.DefaultOutbound}
	}

	// TODO: single send to multiple topics possible?
	for _, topic := range topics {
		if err := c.sendToTopic(r.Text, topic, ntfyMessageModifications{title: "Reminder", late: late}); err != nil {
			return fmt.Errorf("failed to send to topic %s: %w", topic, err)
		}
	}
	return nil
}

func (c *NtfyClient) sendToTopic(text, topic string, mods ...ntfyMessageModifications) error {
	var mod ntfyMessageModifications
	if len(mods) > 0 {
		mod = mods[0]
	}

	if mod.late {
		text = fmt.Sprintf("%s %s", c.cfg.LatePrefix, text)
	}

	url := fmt.Sprintf("%s/%s", strings.TrimRight(c.cfg.Ntfy.Server, "/"), topic)

	req, err := http.NewRequest("POST", url, strings.NewReader(text))
	if err != nil {
		return err
	}

	if mod.late {
		req.Header.Set("Tags", "warning")
	}

	if mod.title != "" {
		req.Header.Set("Title", mod.title)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Ntfy.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned status %d", resp.StatusCode)
	}

	slog.Info("notification sent", "topic", topic)
	return nil
}

func (c *NtfyClient) SubscribeAllWithReconnect() <-chan SubscriptionMessage {
	ch := make(chan SubscriptionMessage)

	if len(c.cfg.Inbound) == 0 {
		slog.Info("no inbound topics configured, skipping subscription")
		return ch
	}

	topics := make([]string, len(c.cfg.Inbound))
	for i, inbound := range c.cfg.Inbound {
		topics[i] = inbound.Topic
	}
	combined := strings.Join(topics, ",")

	go func() {
		for {
			slog.Info("subscribing to ntfy topics", "topics", combined)
			if err := c.subscribe(combined, ch); err != nil {
				slog.Error("ntfy subscription dropped, reconnecting in 5s", "topics", combined, "err", err)
			}
			time.Sleep(5 * time.Second)
		}
	}()

	return ch
}

func (c *NtfyClient) subscribe(topics string, ch chan<- SubscriptionMessage) error {
	url := fmt.Sprintf("%s/%s/json", strings.TrimRight(c.cfg.Ntfy.Server, "/"), topics)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Ntfy.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy subscription failed with status %d", resp.StatusCode)
	}

	slog.Info("successfully subscribed to ntfy topics", "host", c.cfg.Ntfy.Server, "topics", topics)

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
		if msg.Event != "message" {
			continue
		}

		if strings.HasPrefix(msg.Message, "[later]") {
			continue
		}

		outbound := c.resolveOutbound(msg.Topic)
		ch <- SubscriptionMessage{Text: msg.Message, Outbound: outbound, Inbound: msg.Topic}
	}

	return scanner.Err()
}

func (c *NtfyClient) resolveOutbound(topic string) []string {
	for _, inbound := range c.cfg.Inbound {
		if inbound.Topic == topic {
			if len(inbound.Outbound) > 0 {
				return inbound.Outbound
			}
		}
	}
	return []string{c.cfg.Ntfy.DefaultOutbound}
}

func (c *NtfyClient) SendConfirmation(topic string, r *reminder.Reminder) error {
	msg := fmt.Sprintf("Reminder set for %s ✅ (%s)", r.DueAt.Format("Mon Jan 2, 15:04"), r.ID)
	return c.sendSystem(msg, topic)
}

func (c *NtfyClient) sendSystem(text, topic string) error {
	return c.sendToTopic("[later] "+text, topic)
}
