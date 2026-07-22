package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zawnk/later/internal/reminder"
)

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

type client struct {
	baseURL string
	token   string
	httpc   *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpc:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *client) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &apiError{status: resp.StatusCode, message: extractErrorMessage(msg, resp.Status)}
	}
	return resp, nil
}

func extractErrorMessage(body []byte, fallbackStatus string) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fallbackStatus
	}
	return text
}

func (c *client) getJSON(path string, v any) error {
	resp, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

type createRequest struct {
	Text     string
	Topics   []string
	Tags     []string
	Priority string
	Click    string
}

func (c *client) create(req createRequest) (*reminder.Reminder, error) {
	payload, err := json.Marshal(struct {
		Text           string   `json:"text"`
		OutboundTopics []string `json:"outbound_topics,omitempty"`
		Tags           []string `json:"tags,omitempty"`
		Priority       string   `json:"priority,omitempty"`
		Click          string   `json:"click,omitempty"`
	}{Text: req.Text, OutboundTopics: req.Topics, Tags: req.Tags, Priority: req.Priority, Click: req.Click})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(http.MethodPost, "/reminders", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rem reminder.Reminder
	if err := json.NewDecoder(resp.Body).Decode(&rem); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &rem, nil
}

func (c *client) listPending(sortBy string) ([]reminder.Reminder, error) {
	var reminders []reminder.Reminder
	err := c.getJSON("/reminders?sort="+url.QueryEscape(sortBy), &reminders)
	return reminders, err
}

func (c *client) listArchive(limit int) (archived []reminder.ArchivedReminder, total int, err error) {
	resp, err := c.do(http.MethodGet, fmt.Sprintf("/reminders/archive?limit=%d", limit), nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&archived); err != nil {
		return nil, 0, fmt.Errorf("decoding response: %w", err)
	}

	total = len(archived)
	if t, err := strconv.Atoi(resp.Header.Get("X-Total-Count")); err == nil {
		total = t
	}
	return archived, total, nil
}

func (c *client) searchPending(query string) ([]reminder.Reminder, error) {
	var reminders []reminder.Reminder
	err := c.getJSON("/reminders?q="+url.QueryEscape(query), &reminders)
	return reminders, err
}

func (c *client) searchArchive(query string) ([]reminder.ArchivedReminder, error) {
	var archived []reminder.ArchivedReminder
	err := c.getJSON("/reminders/archive?q="+url.QueryEscape(query), &archived)
	return archived, err
}

func (c *client) next() (*reminder.Reminder, error) {
	var rem reminder.Reminder
	if err := c.getJSON("/reminders/next", &rem); err != nil {
		return nil, err
	}
	return &rem, nil
}

func (c *client) last() (*reminder.ArchivedReminder, error) {
	var rem reminder.ArchivedReminder
	if err := c.getJSON("/reminders/last", &rem); err != nil {
		return nil, err
	}
	return &rem, nil
}

func (c *client) cancel(id string) error {
	resp, err := c.do(http.MethodDelete, "/reminders/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *client) postpone(id, duration string) (*reminder.Reminder, error) {
	path := "/reminders/" + url.PathEscape(id) + "/postpone?duration=" + url.QueryEscape(duration)
	resp, err := c.do(http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rem reminder.Reminder
	if err := json.NewDecoder(resp.Body).Decode(&rem); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &rem, nil
}
