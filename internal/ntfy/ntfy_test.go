package ntfy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
)

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
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
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
	c := New(testConfig("http://irrelevant"))

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
	c := New(cfg)

	r := reminder.Reminder{
		ID:             "abc123",
		Text:           "buy milk",
		OutboundTopics: []string{"topic-a"},
		CreatedAt:      time.Now(),
	}

	if err := c.Send(context.Background(), r, false); err != nil {
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
	c := New(testConfig(srv.URL))

	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now()}
	if err := c.Send(context.Background(), r, true); err != nil {
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
		c := New(testConfig(srv.URL))

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-30 * time.Minute)}
		if err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "buy milk" {
			t.Errorf("body = %q, want no age line below the threshold", got)
		}
	})

	t.Run("above the threshold: age line appended as a second line", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL))

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-3 * time.Hour)}
		if err := c.Send(context.Background(), r, false); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "buy milk\n(set 3 hours ago)" {
			t.Errorf("body = %q, want %q", got, "buy milk\n(set 3 hours ago)")
		}
	})

	t.Run("late prefix wraps the whole thing, age line stays at the end", func(t *testing.T) {
		srv, getReqs := recordingServer(t)
		c := New(testConfig(srv.URL))

		r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, CreatedAt: time.Now().Add(-3 * time.Hour)}
		if err := c.Send(context.Background(), r, true); err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		if got := getReqs()[0].body; got != "DELAYED: buy milk\n(set 3 hours ago)" {
			t.Errorf("body = %q, want %q", got, "DELAYED: buy milk\n(set 3 hours ago)")
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
			c := New(testConfig(srv.URL))

			r := reminder.Reminder{Text: "buy cake", OutboundTopics: []string{"topic-a"}, Tags: tt.tags}
			if err := c.Send(context.Background(), r, tt.late); err != nil {
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
			c := New(testConfig(srv.URL))

			r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, Priority: tt.priority}
			if err := c.Send(context.Background(), r, tt.late); err != nil {
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
			c := New(testConfig(srv.URL))

			r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}, Click: tt.click}
			if err := c.Send(context.Background(), r, false); err != nil {
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
	c := New(testConfig(srv.URL))

	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a", "topic-b"}}
	if err := c.Send(context.Background(), r, false); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 2 {
		t.Fatalf("fake server saw %d requests, want 2 (one per topic)", len(reqs))
	}

	if reqs[0].path != "/topic-a" || reqs[1].path != "/topic-b" {
		t.Errorf("request paths = %q, %q; want /topic-a then /topic-b", reqs[0].path, reqs[1].path)
	}
}

func TestSend_NoTopicsIsAnError(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL))

	r := reminder.Reminder{ID: "abc123", Text: "buy milk"}
	err := c.Send(context.Background(), r, false)
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

	c := New(testConfig(srv.URL))
	r := reminder.Reminder{Text: "buy milk", OutboundTopics: []string{"topic-a"}}

	err := c.Send(context.Background(), r, false)
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

func TestSendConfirmation(t *testing.T) {
	srv, getReqs := recordingServer(t)
	c := New(testConfig(srv.URL))

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
	c := New(testConfig(srv.URL))

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

	c := New(testConfig(srv.URL))

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

	c := New(testConfig(srv.URL))
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 4)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return &reminder.Reminder{ID: "rem-1", DueAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 4)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 4)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 4)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return nil, errors.New("no time information found")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 16)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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

	c := New(testConfig(srv.URL))
	c.reconnectWait = time.Millisecond

	created := make(chan createCall, 64)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return &reminder.Reminder{ID: "rem-1"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, create)
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
	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	created := make(chan createCall, 1)
	create := func(msg ParsedInboundMessage) (*reminder.Reminder, error) {
		created <- createCall{text: msg.Text, outbound: msg.Outbound, tags: msg.Tags, priority: msg.Priority}
		return nil, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, create)
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
