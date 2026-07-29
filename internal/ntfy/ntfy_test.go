package ntfy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zawnk/later/internal/actiontoken"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/service"
)

var testActionSecret = []byte("test-action-secret")

type stubReminderService struct {
	createFn  func(service.CreateInput) (*reminder.Reminder, error)
	previewFn func(string) (string, time.Time, error)
}

func (s *stubReminderService) CreateReminder(in service.CreateInput) (*reminder.Reminder, error) {
	if s.createFn == nil {
		return nil, errors.New("CreateReminder not expected in this test")
	}
	return s.createFn(in)
}

func (s *stubReminderService) ParseReminderText(text string) (string, time.Time, error) {
	if s.previewFn == nil {
		return "", time.Time{}, errors.New("ParseReminderText not expected in this test")
	}
	return s.previewFn(text)
}

func TestCutTestPrefix(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantRest string
		wantOK   bool
	}{
		{"bare /test with nothing after", "/test", "", true},
		{"bare /test with surrounding whitespace", "  /test  ", "", true},
		{"/test with text after", "/test buy milk tomorrow", "buy milk tomorrow", true},
		{"case-insensitive, matching phone autocapitalize", "/Test tomorrow", "tomorrow", true},
		{"/TEST all caps", "/TEST tomorrow", "tomorrow", true},
		{"not a trigger at all", "buy milk tomorrow", "", false},
		{"a word merely containing test is not a match", "/testing tomorrow", "", false},
		{"empty string", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, ok := cutTestPrefix(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("cutTestPrefix(%q) ok = %v, want %v", tt.text, ok, tt.wantOK)
			}
			if rest != tt.wantRest {
				t.Errorf("cutTestPrefix(%q) rest = %q, want %q", tt.text, rest, tt.wantRest)
			}
		})
	}
}

type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

func recordingServer(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()

	var mu sync.Mutex
	var reqs []recordedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			header: r.Header.Clone(),
			body:   string(body),
		})
		id := fmt.Sprintf("fake-id-%d", len(reqs))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
	}))
	t.Cleanup(srv.Close)

	getReqs := func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reqs)
	}
	return srv, getReqs
}

func testConfig(serverURL string) *config.Config {
	return &config.Config{
		Ntfy: config.NtfyConfig{
			Server: serverURL,
			Token:  "tk_test_token",
		},
		LatePrefix: "DELAYED:",
		Inbound: []config.Inbound{
			{Topic: "inbound-a", Outbound: []string{"out-1", "out-2"}},
			{Topic: "inbound-b", Outbound: []string{"out-3"}},
		},
	}
}

type createCall struct {
	text     string
	outbound []string
	tags     []string
	priority string
}

func TestResolveOutbound(t *testing.T) {
	c := New(testConfig("http://irrelevant"), testActionSecret, &stubReminderService{})

	tests := []struct {
		name  string
		topic string
		want  []string
	}{
		{"topic with configured outbound list", "inbound-a", []string{"out-1", "out-2"}},
		{"second topic resolves to its own list", "inbound-b", []string{"out-3"}},
		{"unlikely unknown topic resolves to nil (message gets dropped)", "never-heard-of-it", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.resolveOutbound(tt.topic)
			if !slices.Equal(got, tt.want) {
				t.Errorf("resolveOutbound(%q) = %v, want %v", tt.topic, got, tt.want)
			}
		})
	}
}

func TestSend(t *testing.T) {
	srv, getReqs := recordingServer(t)
	cfg := testConfig(srv.URL)
	c := New(cfg, testActionSecret, &stubReminderService{})

	r := reminder.Reminder{
		ID:             "abc123",
		Text:           "buy milk",
		OutboundTopics: []string{"topic-a"},
		CreatedAt:      time.Now(),
	}

	if _, err := c.Send(context.Background(), r, false); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("fake server saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]

	if req.path != "/topic-a" {
		t.Errorf("request path = %q, want %q", req.path, "/topic-a")
	}

	if req.method != http.MethodPost {
		t.Errorf("request method = %q, want POST", req.method)
	}

	if req.body != "buy milk" {
		t.Errorf("request body = %q, want %q", req.body, "buy milk")
	}

	if got := req.header.Get("Title"); got != "Reminder" {
		t.Errorf("Title header = %q, want %q", got, "Reminder")
	}

	if got := req.header.Get("Authorization"); got != "Bearer tk_test_token" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer tk_test_token")
	}

	if got := req.header.Get("Tags"); got != "" {
		t.Errorf("Tags header = %q, want it unset for an on-time reminder", got)
	}
}

func TestSend_Late(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now()}
	if _, err := c.Send(context.Background(), r, true); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("fake server saw %d requests, want 1", len(reqs))
	}

	if reqs[0].body != "DELAYED: buy milk" {
		t.Errorf("late body = %q, want %q", reqs[0].body, "DELAYED: buy milk")
	}

	if got := reqs[0].header.Get("Tags"); got != "warning" {
		t.Errorf("Tags header = %q, want %q", got, "warning")
	}
}

func TestSend_AgeLine(t *testing.T) {
	t.Run("below the threshold: no age line", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-30 * time.Minute)}
		if _, err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "buy milk" {
			t.Errorf("body = %q, want no age line below the threshold", got)
		}
	})

	t.Run("above the threshold: age line appended as a second line", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-3 * time.Hour)}
		if _, err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "buy milk\n(set 3 hours ago)" {
			t.Errorf("body = %q, want %q", got, "buy milk\n(set 3 hours ago)")
		}
	})

	t.Run("late prefix wraps the whole thing, age line stays at the end", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-3 * time.Hour)}
		if _, err := c.Send(context.Background(), r, true); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "DELAYED: buy milk\n(set 3 hours ago)" {
			t.Errorf("body = %q, want %q", got, "DELAYED: buy milk\n(set 3 hours ago)")
		}
	})
}

func TestSend_ActionButtons(t *testing.T) {
	t.Run("no base_url configured: no Actions header", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

		r := reminder.Reminder{ID: "abc123", Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now()}
		if _, err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].header.Get("Actions"); got != "" {
			t.Errorf("Actions header = %q, want empty when base_url is unset", got)
		}
	})

	t.Run("base_url configured: two action buttons attached with verifiable tokens", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		cfg := testConfig(srv.URL)
		cfg.Server.BaseURL = "https://later.example.com"
		c := New(cfg, testActionSecret, &stubReminderService{})

		r := reminder.Reminder{ID: "abc123", Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now()}
		if _, err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		actions := getReqs()[0].header.Get("Actions")
		if actions == "" {
			t.Fatal("Actions header is empty, want two action buttons")
		}
		if !strings.Contains(actions, "Snooze 1h") || !strings.Contains(actions, "Tomorrow") {
			t.Errorf("Actions header = %q, want both button labels", actions)
		}
		if !strings.Contains(actions, "https://later.example.com/reminders/abc123/postpone?duration=in+1h") {
			t.Errorf("Actions header = %q, want the Snooze 1h button's callback URL", actions)
		}
		if !strings.Contains(actions, "https://later.example.com/reminders/abc123/postpone?duration=tomorrow+morning") {
			t.Errorf("Actions header = %q, want the Tomorrow button's callback URL", actions)
		}
		if got := strings.Count(actions, "method=POST"); got != 2 {
			t.Errorf("Actions header has %d method=POST occurrences, want 2 (one per button): %q", got, actions)
		}

		tokenRe := regexp.MustCompile(`headers\.Authorization=Bearer (\S+?),`)
		matches := tokenRe.FindAllStringSubmatch(actions, -1)
		if len(matches) != 2 {
			t.Fatalf("found %d bearer tokens in Actions header, want 2 (got: %q)", len(matches), actions)
		}
		if matches[0][1] != matches[1][1] {
			t.Error("the two buttons carry different tokens, want one shared token (the postpone duration, not the token, is what differs between them)")
		}

		if _, err := actiontoken.Verify(testActionSecret, matches[0][1], "abc123", "postpone"); err != nil {
			t.Errorf("minted token failed to verify: %v", err)
		}
	})

	t.Run("bare IP:port base_url (no scheme) still produces a usable callback URL", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		cfg := testConfig(srv.URL)
		cfg.Server.BaseURL = "192.168.1.53:8080"
		c := New(cfg, testActionSecret, &stubReminderService{})

		r := reminder.Reminder{ID: "abc123", Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now()}
		if _, err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		actions := getReqs()[0].header.Get("Actions")
		if !strings.Contains(actions, "http://192.168.1.53:8080/reminders/abc123/postpone") {
			t.Errorf("Actions header = %q, want the callback URL normalized to include http://", actions)
		}
	})
}

func TestSend_Tags(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		late     bool
		wantTags string
	}{
		{"no tags, not late", nil, false, ""},
		{"custom tags pass through comma-separated", []string{"partying_face", "birthday"}, false, "partying_face,birthday"},
		{"late prepends warning to custom tags", []string{"birthday"}, true, "warning,birthday"},
		{"late with user-supplied warning does not duplicate it", []string{"birthday", "warning"}, true, "birthday,warning"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, getReqs := recordingServer(t)
			c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

			r := reminder.Reminder{Text: "buy cake", OutboundTopics: []string{"topic-a"}, Tags: tt.tags}
			if _, err := c.Send(context.Background(), r, tt.late); err != nil {
				t.Fatalf("Send() error = %v", err)
			}

			reqs := getReqs()
			if len(reqs) != 1 {
				t.Fatalf("fake server saw %d requests, want 1", len(reqs))
			}

			if got := reqs[0].header.Get("Tags"); got != tt.wantTags {
				t.Errorf("Tags header = %q, want %q", got, tt.wantTags)
			}
		})
	}
}

func TestSend_Priority(t *testing.T) {
	tests := []struct {
		name         string
		priority     string
		late         bool
		wantPriority string
	}{
		{"no priority, not late: no header", "", false, ""},
		{"explicit priority passes through", "low", false, "low"},
		{"late bumps unset priority to high", "", true, "high"},
		{"late bumps low to high", "low", true, "high"},
		{"late keeps urgent", "urgent", true, "urgent"},
		{"late bump works on digit form too", "2", true, "high"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, getReqs := recordingServer(t)
			c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

			r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, Priority: tt.priority}
			if _, err := c.Send(context.Background(), r, tt.late); err != nil {
				t.Fatalf("Send() error = %v", err)
			}

			reqs := getReqs()
			if len(reqs) != 1 {
				t.Fatalf("fake server saw %d requests, want 1", len(reqs))
			}

			if got := reqs[0].header.Get("Priority"); got != tt.wantPriority {
				t.Errorf("Priority header = %q, want %q", got, tt.wantPriority)
			}
		})
	}
}

func TestSend_Click(t *testing.T) {
	tests := []struct {
		name      string
		click     string
		wantClick string
	}{
		{"no click: no header", "", ""},
		{"click URL is sent as the Click header", "https://example.com/x", "https://example.com/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, getReqs := recordingServer(t)
			c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

			r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, Click: tt.click}
			if _, err := c.Send(context.Background(), r, false); err != nil {
				t.Fatalf("Send() error = %v", err)
			}

			reqs := getReqs()
			if len(reqs) != 1 {
				t.Fatalf("fake server saw %d requests, want 1", len(reqs))
			}

			if got := reqs[0].header.Get("Click"); got != tt.wantClick {
				t.Errorf("Click header = %q, want %q", got, tt.wantClick)
			}
		})
	}
}

func TestSend_MultipleTopics(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a", "topic-b"}}
	ids, err := c.Send(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 2 {
		t.Fatalf("fake server saw %d requests, want 2 (one per topic)", len(reqs))
	}

	if reqs[0].path != "/topic-a" || reqs[1].path != "/topic-b" {
		t.Errorf("request paths = %q, %q; want /topic-a then /topic-b", reqs[0].path, reqs[1].path)
	}

	if len(ids) != 2 {
		t.Fatalf("Send() returned %d ids, want 2 (one per topic): %+v", len(ids), ids)
	}

	if ids["topic-a"] != "fake-id-1" || ids["topic-b"] != "fake-id-2" {
		t.Errorf("Send() ids = %+v, want topic-a -> fake-id-1 and topic-b -> fake-id-2 (each topic must keep its own id, not the last one written)", ids)
	}
}

func TestSend_NoTopicsIsAnError(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	r := reminder.Reminder{ID: "abc123", Text: "buy milk"}
	_, err := c.Send(context.Background(), r, false)
	if err == nil {
		t.Fatal("Send() error = nil, want an error for a reminder without topics")
	}

	if !strings.Contains(err.Error(), "no outbound topics") {
		t.Errorf("Send() error = %q, want it to say the reminder has no outbound topics", err)
	}

	if reqs := getReqs(); len(reqs) != 0 {
		t.Errorf("fake server saw %d requests, want 0", len(reqs))
	}
}

func TestSend_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("access denied"))
	}))
	t.Cleanup(srv.Close)

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})
	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}}

	_, err := c.Send(context.Background(), r, false)
	if err == nil {
		t.Fatal("Send() error = nil, want an error for a 403 response")
	}

	if !strings.Contains(err.Error(), "403") {
		t.Errorf("Send() error = %q, want it to mention the status code 403", err)
	}

	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("Send() error = %q, want it to include the response body", err)
	}

	if !strings.Contains(err.Error(), "topic-a") {
		t.Errorf("Send() error = %q, want it to name the failing topic", err)
	}
}

func TestSend_MissingIDIsAnError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"valid JSON but no id field", `{"time":1673542291,"event":"message"}`},
		{"id present but empty", `{"id":""}`},
		{"not JSON at all", "not json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})
			r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}}

			ids, err := c.Send(context.Background(), r, false)
			if err == nil {
				t.Fatalf("Send() error = nil, ids = %+v, want an error for a 2xx response with no usable message id", ids)
			}

			if !strings.Contains(err.Error(), "topic-a") {
				t.Errorf("Send() error = %q, want it to name the failing topic", err)
			}
		})
	}
}

func TestSendConfirmation(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	r := &reminder.Reminder{
		ID:    "abc123",
		DueAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC),
	}
	if err := c.sendConfirmation(context.Background(), "inbound-a", r); err != nil {
		t.Fatalf("sendConfirmation() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("fake server saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]

	if req.path != "/inbound-a" {
		t.Errorf("request path = %q, want %q", req.path, "/inbound-a")
	}

	if !strings.HasPrefix(req.body, "[later] ") {
		t.Errorf("confirmation body = %q, want the %q prefix (loop prevention)", req.body, "[later] ")
	}

	if !strings.Contains(req.body, "abc123") {
		t.Errorf("confirmation body = %q, want it to contain the reminder ID", req.body)
	}

	if !strings.Contains(req.body, "Mon Jun 15, 09:00") {
		t.Errorf("confirmation body = %q, want it to contain the formatted due time", req.body)
	}

	if got := req.header.Get("Title"); got != "" {
		t.Errorf("Title header = %q -- confirmations currently have no title", got)
	}
}

func TestSendError(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	if err := c.sendError(context.Background(), "inbound-a", errors.New("no time information found")); err != nil {
		t.Fatalf("sendError() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("fake server saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]

	if req.path != "/inbound-a" {
		t.Errorf("request path = %q, want %q", req.path, "/inbound-a")
	}

	if !strings.HasPrefix(req.body, "[later] error: ") {
		t.Errorf("error body = %q, want the %q prefix (loop prevention + error marker)", req.body, "[later] error: ")
	}

	if !strings.Contains(req.body, "no time information found") {
		t.Errorf("error body = %q, want it to contain the create error", req.body)
	}
}

func TestSubscribe(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"keepalive","topic":"inbound-a"}`,
		`{"event":"open","topic":"inbound-a"}`,
		`this is not json`,
		`{"event":"message","topic":"inbound-a","message":"buy milk in 3 days"}`,
		`{"event":"message","topic":"inbound-a","message":"[later] Reminder set for ..."}`,
		`{"event":"message","topic":"never-heard-of-it","message":"should be dropped"}`,
		`{"event":"message","topic":"inbound-b","message":"water plants tomorrow"}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbound-a,inbound-b/json" {
			t.Errorf("subscribe path = %q, want %q", r.URL.Path, "/inbound-a,inbound-b/json")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tk_test_token" {
			t.Errorf("subscribe Authorization header = %q, want %q", got, "Bearer tk_test_token")
		}
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})

	msgs := make(chan subscriptionMessage, 16)
	if _, err := c.subscribe(context.Background(), "inbound-a,inbound-b", "", msgs); err != nil {
		t.Fatalf("subscribe() error = %v", err)
	}
	close(msgs)

	var got []subscriptionMessage
	for m := range msgs {
		got = append(got, m)
	}

	if len(got) != 2 {
		t.Fatalf("subscribe delivered %d messages, want 2; got: %+v", len(got), got)
	}

	if got[0].Text != "buy milk in 3 days" {
		t.Errorf("first message Text = %q, want %q", got[0].Text, "buy milk in 3 days")
	}

	if got[0].Inbound != "inbound-a" {
		t.Errorf("first message Inbound = %q, want %q", got[0].Inbound, "inbound-a")
	}

	if !slices.Equal(got[0].Outbound, []string{"out-1", "out-2"}) {
		t.Errorf("first message Outbound = %v, want the topic's configured outbound list", got[0].Outbound)
	}

	if got[1].Text != "water plants tomorrow" {
		t.Errorf("second message Text = %q, want %q", got[1].Text, "water plants tomorrow")
	}

	if !slices.Equal(got[1].Outbound, []string{"out-3"}) {
		t.Errorf("second message Outbound = %v, want inbound-b's configured outbound list", got[1].Outbound)
	}
}

func TestSubscribe_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token"))
	}))
	t.Cleanup(srv.Close)

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{})
	msgs := make(chan subscriptionMessage, 1)

	_, err := c.subscribe(context.Background(), "inbound-a", "", msgs)
	if err == nil {
		t.Fatal("subscribe() error = nil, want an error for a 401 response")
	}

	if !strings.Contains(err.Error(), "401") {
		t.Errorf("subscribe() error = %q, want it to mention the status code 401", err)
	}

	if !strings.Contains(err.Error(), "bad token") {
		t.Errorf("subscribe() error = %q, want it to include the response body", err)
	}
}

func TestRun_CreatesAndConfirms(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)
	confirmed := make(chan recordedRequest, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			confirmed <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"buy milk in 3 days"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 4)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return &reminder.Reminder{ID: "rem-1", DueAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)}, nil
	}

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case call := <-created:
		if call.text != "buy milk in 3 days" {
			t.Errorf("create text = %q, want %q", call.text, "buy milk in 3 days")
		}
		if !slices.Equal(call.outbound, []string{"out-1", "out-2"}) {
			t.Errorf("create outbound = %v, want inbound-a's configured list", call.outbound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the create callback")
	}

	select {
	case conf := <-confirmed:
		if conf.path != "/inbound-a" {
			t.Errorf("confirmation path = %q, want /inbound-a (the message's own topic)", conf.path)
		}
		if !strings.HasPrefix(conf.body, "[later] ") {
			t.Errorf("confirmation body = %q, want the %q prefix (loop prevention)", conf.body, "[later] ")
		}
		if !strings.Contains(conf.body, "rem-1") {
			t.Errorf("confirmation body = %q, want it to contain the created reminder's id", conf.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the confirmation POST")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_TestParseRepliesWithPreview(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)
	replied := make(chan recordedRequest, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			replied <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"/test buy milk tomorrow #work"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		t.Error("create was called, want the /test trigger to bypass reminder creation entirely")
		return nil, errors.New("unexpected create call")
	}
	preview := func(text string) (string, time.Time, error) {
		if text != "buy milk tomorrow" {
			t.Errorf("preview received %q, want the #work directive already stripped (\"buy milk tomorrow\")", text)
		}
		return "buy milk", time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC), nil
	}

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create, previewFn: preview})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case reply := <-replied:
		if reply.path != "/inbound-a" {
			t.Errorf("reply path = %q, want /inbound-a (the message's own topic)", reply.path)
		}
		if !strings.HasPrefix(reply.body, "[later] ") {
			t.Errorf("reply body = %q, want the %q prefix", reply.body, "[later] ")
		}
		if !strings.Contains(reply.body, "buy milk") {
			t.Errorf("reply body = %q, want it to contain the previewed task text", reply.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the preview reply")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_TestParseFailureSendsErrorFeedback(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)
	errored := make(chan recordedRequest, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			errored <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"/test gibberish"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		t.Error("create was called, want the /test trigger to bypass reminder creation entirely")
		return nil, errors.New("unexpected create call")
	}
	preview := func(text string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("no time information found")
	}

	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create, previewFn: preview})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case reply := <-errored:
		if !strings.HasPrefix(reply.body, "[later] error:") {
			t.Errorf("reply body = %q, want the %q prefix", reply.body, "[later] error:")
		}
		if !strings.Contains(reply.body, "no time information found") {
			t.Errorf("reply body = %q, want it to contain the real parse failure reason", reply.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the error reply")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_CreatesWithDirectives(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"buy milk in 3 days #groceries !high"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 4)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case call := <-created:
		if call.text != "buy milk in 3 days" {
			t.Errorf("create text = %q, want the directives stripped: %q", call.text, "buy milk in 3 days")
		}
		if !slices.Equal(call.tags, []string{"groceries"}) {
			t.Errorf("create tags = %v, want %v", call.tags, []string{"groceries"})
		}
		if call.priority != "high" {
			t.Errorf("create priority = %q, want %q", call.priority, "high")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the create callback")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_ConflictingPriorityDirectivesSendsErrorFeedback(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)
	errored := make(chan recordedRequest, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			errored <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"buy milk in 3 days !high !low"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 4)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case req := <-errored:
		if req.path != "/inbound-a" {
			t.Errorf("error feedback path = %q, want /inbound-a (the message's own topic)", req.path)
		}
		if !strings.HasPrefix(req.body, "[later] error: ") {
			t.Errorf("error feedback body = %q, want it to start with %q", req.body, "[later] error: ")
		}
		if !strings.Contains(req.body, "multiple priority directives") {
			t.Errorf("error feedback body = %q, want it to mention the conflict", req.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the error-feedback POST")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if len(created) != 0 {
		t.Errorf("create was called %d times, want 0 -- a directive conflict must be rejected before create is ever invoked", len(created))
	}
}

func TestRun_CreateFailureSendsErrorFeedback(t *testing.T) {
	var (
		mu        sync.Mutex
		delivered bool
	)
	errored := make(chan recordedRequest, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			errored <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
			return
		}

		mu.Lock()
		first := !delivered
		delivered = true
		mu.Unlock()
		if first {
			_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"gibberish"}`+"\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 4)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return nil, errors.New("no time information found")
	}
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the create callback")
	}

	select {
	case req := <-errored:
		if req.path != "/inbound-a" {
			t.Errorf("error feedback path = %q, want /inbound-a (the message's own topic)", req.path)
		}
		if !strings.HasPrefix(req.body, "[later] error: ") {
			t.Errorf("error feedback body = %q, want it to start with %q (loop prevention + error marker)", req.body, "[later] error: ")
		}
		if !strings.Contains(req.body, "no time information found") {
			t.Errorf("error feedback body = %q, want it to contain the create error", req.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the error-feedback POST")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_ReconnectsAfterStreamDrops(t *testing.T) {
	var mu sync.Mutex
	connections := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			return
		}
		mu.Lock()
		connections++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"event":"message","topic":"inbound-a","message":"hi"}`+"\n")
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 16)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	for i := range 2 {
		select {
		case call := <-created:
			if call.text != "hi" {
				t.Errorf("create %d text = %q, want %q", i, call.text, "hi")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for create %d -- Run did not reconnect?", i)
		}
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if connections < 2 {
		t.Errorf("server saw %d subscribe connections, want at least 2 (reconnect)", connections)
	}
}

func TestRun_ResubscribesWithSince(t *testing.T) {
	var mu sync.Mutex
	var sinceParams []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			return
		}
		mu.Lock()
		sinceParams = append(sinceParams, r.URL.Query().Get("since"))
		n := len(sinceParams)
		mu.Unlock()

		if n == 1 {
			_, _ = io.WriteString(w, `{"id":"msg-1","event":"message","topic":"inbound-a","message":"hi"}`+"\n")
			return
		}

		_, _ = io.WriteString(w, `{"id":"msg-2","event":"message","topic":"inbound-a","message":"there"}`+"\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	created := make(chan createCall, 64)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}
	c := New(testConfig(srv.URL), testActionSecret, &stubReminderService{createFn: create})
	c.reconnectWait = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx)
	}()

	var texts []string
	for i := range 2 {
		select {
		case call := <-created:
			texts = append(texts, call.text)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for create %d", i)
		}
	}
	if !slices.Equal(texts, []string{"hi", "there"}) {
		t.Errorf("created texts = %v, want [hi there] -- the reconnect must not lose \"there\"", texts)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sinceParams) < 2 {
		t.Fatalf("saw %d connections, want at least 2", len(sinceParams))
	}
	if sinceParams[0] != "" {
		t.Errorf("first connection since = %q, want empty (no since= on first connect)", sinceParams[0])
	}
	if sinceParams[1] != "msg-1" {
		t.Errorf("second connection since = %q, want %q (resume from the last seen message ID)", sinceParams[1], "msg-1")
	}
}

func TestRun_NoInboundTopics(t *testing.T) {
	srv, getReqs := recordingServer(t)
	cfg := testConfig(srv.URL)
	cfg.Inbound = nil

	created := make(chan createCall, 1)
	create := func(in service.CreateInput) (*reminder.Reminder, error) {
		created <- createCall{text: in.Text, outbound: in.OutboundTopics, tags: in.Tags, priority: in.Priority}
		return nil, nil
	}
	c := New(cfg, testActionSecret, &stubReminderService{createFn: create})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on a cancelled context")
	case <-done:
	}

	if len(created) != 0 {
		t.Error("create callback was called, want no calls without inbound topics")
	}

	if reqs := getReqs(); len(reqs) != 0 {
		t.Errorf("server saw %d requests, want 0 when no inbound topics are configured", len(reqs))
	}
}
