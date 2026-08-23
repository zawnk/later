package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/zawnk/later/internal/reminder"
)

func runCLI(t *testing.T, a *app, args ...string) error {
	t.Helper()
	var cli CLI
	parser, err := kong.New(&cli, kong.Name("later"))

	if err != nil {
		t.Fatalf("kong.New() error = %v (broken grammar struct)", err)
	}
	ctx, err := parser.Parse(args)

	if err != nil {
		return err
	}
	a.json = cli.JSON
	return ctx.Run(a)
}

func TestClientCreate(t *testing.T) {
	want := reminder.Reminder{
		ID:    "abc123",
		Text:  "buy milk",
		DueAt: time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/reminders" {
			t.Errorf("request = %s %s, want POST /reminders", r.Method, r.URL.Path)
		}

		if got := r.Header.Get("Authorization"); got != "Bearer tk_test" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tk_test")
		}
		body, _ := io.ReadAll(r.Body)

		if string(body) != `{"text":"buy milk in 3 days"}` {
			t.Errorf("body = %s, want text-only JSON", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(want)
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, "tk_test")
	got, err := c.create(createRequest{Text: "buy milk in 3 days"})

	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	if got.ID != want.ID || got.Text != want.Text || !got.DueAt.Equal(want.DueAt) {
		t.Errorf("create() = %+v, want %+v", got, want)
	}
}

func TestClientSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reminder abc123 is still pending, cannot postpone", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, "tk_test")
	_, err := c.postpone("abc123", "1d")

	if err == nil {
		t.Fatal("postpone() error = nil, want the server's message")
	}

	if !strings.Contains(err.Error(), "still pending") {
		t.Errorf("postpone() error = %q, want the server's message surfaced verbatim", err)
	}
}

func TestClientSurfacesJSONServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"reminder abc123 is still pending, cannot postpone"}`))
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, "tk_test")
	_, err := c.postpone("abc123", "1d")

	if err == nil {
		t.Fatal("postpone() error = nil, want the server's message")
	}

	if strings.Contains(err.Error(), "{") {
		t.Errorf("postpone() error = %q, want the extracted message, not the raw JSON envelope", err)
	}

	if !strings.Contains(err.Error(), "still pending") {
		t.Errorf("postpone() error = %q, want the server's message extracted from the JSON envelope", err)
	}
}

func TestFreeTextCreatesReminder(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123", Text: "buy milk"})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "in", "3", "hours", "buy", "milk"); err != nil {
		t.Fatalf("runCLI() error = %v", err)
	}

	if gotBody != `{"text":"in 3 hours buy milk"}` {
		t.Errorf("server received body %s, want the args joined into text", gotBody)
	}

	if !strings.Contains(out.String(), "abc123") {
		t.Errorf("output = %q, want it to mention the new reminder id", out.String())
	}
}

func TestFreeTextWithTrailingTopicFlag(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "in", "3", "hours", "call", "paternal-grandma", "--topic=family-reminders"); err != nil {
		t.Fatalf("runCLI() error = %v", err)
	}
	want := `{"text":"in 3 hours call paternal-grandma","outbound_topics":["family-reminders"]}`

	if gotBody != want {
		t.Errorf("server received body %s, want %s", gotBody, want)
	}
}

func TestLeadingDashTextFailsLoudly(t *testing.T) {
	a := &app{out: io.Discard, url: "http://irrelevant", token: "tk_test"}

	if err := runCLI(t, a, "in", "3", "hours", "-important", "thing"); err == nil {
		t.Error("runCLI() with a leading-dash text token error = nil, want a loud parse error")
	}
}

func TestDashDashEscapeHatch(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "--", "temp", "is", "-5", "degrees", "tomorrow"); err != nil {
		t.Fatalf("runCLI() with -- escape error = %v", err)
	}

	if gotBody != `{"text":"temp is -5 degrees tomorrow"}` {
		t.Errorf("server received body %s, want the post--- tokens as literal text", gotBody)
	}
}

func TestSubcommandDispatch(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list"); err != nil {
		t.Fatalf(`runCLI("list") error = %v`, err)
	}

	if gotMethod != http.MethodGet || gotPath != "/reminders" {
		t.Errorf(`"list" made %s %s, want GET /reminders`, gotMethod, gotPath)
	}

	if !strings.Contains(out.String(), "no pending reminders") {
		t.Errorf(`"list" output = %q, want the empty-list message`, out.String())
	}

	if err := runCLI(t, a, "cancel", "abc123"); err != nil {
		t.Fatalf(`runCLI("cancel") error = %v`, err)
	}

	if gotMethod != http.MethodDelete || gotPath != "/reminders/abc123" {
		t.Errorf(`"cancel" made %s %s, want DELETE /reminders/abc123`, gotMethod, gotPath)
	}
}

func TestMissingArgsRejectedByParser(t *testing.T) {
	a := &app{out: io.Discard, url: "http://irrelevant", token: "tk_test"}

	if err := runCLI(t, a, "postpone", "abc123"); err == nil {
		t.Error(`runCLI("postpone" without duration) error = nil, want a parse error`)
	}

	if err := runCLI(t, a, "cancel"); err == nil {
		t.Error(`runCLI("cancel" without id) error = nil, want a parse error`)
	}
}

func TestMissingTokenFailsEverythingButHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: ""}

	err := runCLI(t, a, "list")

	if err == nil || !strings.Contains(err.Error(), "LATER_TOKEN") {
		t.Errorf(`runCLI("list") without token error = %v, want a LATER_TOKEN message`, err)
	}

	if err := runCLI(t, a, "healthcheck"); err != nil {
		t.Errorf(`runCLI("healthcheck") without token error = %v, want nil`, err)
	}
}

func TestHealthcheckUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: ""}

	if err := runCLI(t, a, "healthcheck"); err == nil {
		t.Error(`runCLI("healthcheck") against a failing server error = nil, want an error (exit 1 in main)`)
	}
}

func TestListSorting(t *testing.T) {
	var gotQuery string
	stored := []reminder.Reminder{
		{ID: "first-in-server-response"},
		{ID: "second-in-server-response"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(stored)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list"); err != nil {
		t.Fatalf(`runCLI("list") error = %v`, err)
	}

	if gotQuery != "sort=due" {
		t.Errorf("default list query = %q, want %q", gotQuery, "sort=due")
	}

	if first := strings.SplitN(out.String(), "\n", 2)[0]; !strings.HasPrefix(first, "first-in-server-response") {
		t.Errorf("output order = %q, want the server's response order preserved (no client-side re-sort)", out.String())
	}

	out.Reset()

	if err := runCLI(t, a, "list", "--by=create"); err != nil {
		t.Fatalf(`runCLI("list --by=create") error = %v`, err)
	}

	if gotQuery != "sort=create" {
		t.Errorf("--by=create query = %q, want %q", gotQuery, "sort=create")
	}

	if err := runCLI(t, a, "list", "--by=nonsense"); err == nil {
		t.Error(`runCLI("list --by=nonsense") error = nil, want an enum validation error`)
	}
}

func TestListVerboseGroupsByDueETA(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{ID: "overdue1", Text: "overdue task", DueAt: today.Add(-1 * time.Hour)},
		{ID: "today1", Text: "today task", DueAt: today.Add(23*time.Hour + 59*time.Minute)},
		{ID: "tomorrow1", Text: "tomorrow task", DueAt: today.Add(25 * time.Hour)},
		{ID: "later1", Text: "later task", DueAt: today.Add(10 * 24 * time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --verbose") error = %v`, err)
	}
	got := out.String()

	wantOrder := []string{"Overdue", "overdue1", "Today", "today1", "Tomorrow", "tomorrow1", "Later", "later1"}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := strings.Index(got, want)
		if idx == -1 {
			t.Fatalf("list --verbose output = %q, want to find %q", got, want)
		}
		if idx < lastIdx {
			t.Errorf("list --verbose output = %q, want %q to appear after the previous entries in bucket order", got, want)
		}
		lastIdx = idx
	}
}

func TestListVerboseOmitsEmptyBuckets(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{ID: "today1", Text: "today task", DueAt: today.Add(23*time.Hour + 59*time.Minute)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --verbose") error = %v`, err)
	}
	got := out.String()

	if !strings.Contains(got, "Today") {
		t.Errorf("list --verbose output = %q, want a Today bucket header", got)
	}
	for _, absent := range []string{"Overdue", "Tomorrow", "Later"} {
		if strings.Contains(got, absent) {
			t.Errorf("list --verbose output = %q, want no %q header since that bucket is empty", got, absent)
		}
	}
}

func TestListVerboseGroupsByCreateETA(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{ID: "today1", Text: "created today", CreatedAt: now},
		{ID: "yesterday1", Text: "created yesterday", CreatedAt: today.Add(-12 * time.Hour)},
		{ID: "lastweek1", Text: "created last week", CreatedAt: today.Add(-3*24*time.Hour - 12*time.Hour)},
		{ID: "lastmonth1", Text: "created last month", CreatedAt: today.Add(-14*24*time.Hour - 12*time.Hour)},
		{ID: "earlier1", Text: "created earlier", CreatedAt: today.Add(-39*24*time.Hour - 12*time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--by=create", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --by=create --verbose") error = %v`, err)
	}
	got := out.String()

	wantOrder := []string{
		"Today", "today1",
		"Yesterday", "yesterday1",
		"Last week", "lastweek1",
		"Last month", "lastmonth1",
		"Earlier", "earlier1",
	}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := strings.Index(got, want)
		if idx == -1 {
			t.Fatalf("list --by=create --verbose output = %q, want to find %q", got, want)
		}
		if idx < lastIdx {
			t.Errorf("list --by=create --verbose output = %q, want %q to appear after the previous entries in bucket order", got, want)
		}
		lastIdx = idx
	}
}

func TestListVerboseCreateGroupingUsesDueOwnProximityForFormat(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{
			ID:        "faraway1",
			Text:      "renewal three months out",
			CreatedAt: today.Add(-12 * time.Hour), // lands in the "Yesterday" create bucket
			DueAt:     today.Add(90 * 24 * time.Hour),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--by=create", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --by=create --verbose") error = %v`, err)
	}
	got := out.String()

	if !strings.Contains(got, "Yesterday") {
		t.Fatalf("output = %q, want the row grouped under the Yesterday (create) bucket", got)
	}

	wantDue := formatTime(reminders[0].DueAt)
	if !strings.Contains(got, wantDue) {
		t.Errorf("output = %q, want the due column to show the full date (%q) since the due date is 3 months out, regardless of which create-bucket the row is grouped under", got, wantDue)
	}
}

func TestListVerboseShowsPriorityTagsTopics(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{
			ID:             "abc123",
			Text:           "renew certificate",
			DueAt:          today.Add(23*time.Hour + 59*time.Minute),
			Priority:       "urgent",
			Tags:           []string{"work", "cert"},
			OutboundTopics: []string{"ops-alerts", "backup-topic"},
		},
		{
			ID:    "def456",
			Text:  "pick up dry cleaning",
			DueAt: today.Add(23*time.Hour + 58*time.Minute),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --verbose") error = %v`, err)
	}
	got := out.String()

	if !strings.Contains(got, "urgent") || !strings.Contains(got, "#work #cert") || !strings.Contains(got, "→ops-alerts,backup-topic") {
		t.Errorf("list --verbose output = %q, want priority/tags/topics rendered for abc123", got)
	}

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "def456") {
			if !strings.Contains(line, "-") {
				t.Errorf("list --verbose line = %q, want empty priority/tags/topics cells rendered as -", line)
			}
		}
	}
}

func TestListVerboseOmitsAllEmptyColumns(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{ID: "abc123", Text: "renew certificate", DueAt: today.Add(23*time.Hour + 59*time.Minute)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "list", "--verbose"); err != nil {
		t.Fatalf(`runCLI("list --verbose") error = %v`, err)
	}
	got := out.String()

	if strings.Contains(got, "-") {
		t.Errorf("list --verbose output = %q, want no dash placeholders when priority/tags/topics are entirely absent", got)
	}
}

func TestListVerboseIsNoOpUnderJSON(t *testing.T) {
	now := time.Now()
	reminders := []reminder.Reminder{
		{ID: "abc123", Text: "renew certificate", DueAt: now.Add(1 * time.Hour), Priority: "urgent"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "--json", "list", "--verbose"); err != nil {
		t.Fatalf(`runCLI("--json list --verbose") error = %v`, err)
	}

	var got []reminder.Reminder
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf(`"--json list --verbose" output is not valid JSON: %v; output: %q`, err, out.String())
	}
	if len(got) != 1 || got[0].Priority != "urgent" {
		t.Errorf(`"--json list --verbose" decoded to %+v, want the full reminder unaffected by --verbose`, got)
	}
}

func TestSearchPendingVerbose(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	reminders := []reminder.Reminder{
		{
			ID:       "abc123",
			Text:     "renew certificate",
			DueAt:    today.Add(23*time.Hour + 59*time.Minute),
			Priority: "urgent",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reminders)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "search", "certificate", "--verbose"); err != nil {
		t.Fatalf(`runCLI("search certificate --verbose") error = %v`, err)
	}
	got := out.String()

	if !strings.Contains(got, "Today") {
		t.Errorf("search --verbose output = %q, want a Today bucket header", got)
	}
	if !strings.Contains(got, "urgent") {
		t.Errorf("search --verbose output = %q, want the priority column rendered", got)
	}
}

func TestNextEmptyIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no pending reminders", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "next"); err != nil {
		t.Fatalf(`runCLI("next") on empty error = %v, want nil (exit 0)`, err)
	}

	if !strings.Contains(out.String(), "no pending reminders") {
		t.Errorf(`"next" output = %q, want the server's empty-state message`, out.String())
	}

	out.Reset()
	if err := runCLI(t, a, "--json", "next"); err != nil {
		t.Fatalf(`runCLI("--json next") on empty error = %v`, err)
	}

	if strings.TrimSpace(out.String()) != "null" {
		t.Errorf(`"--json next" output = %q, want null`, out.String())
	}
}

func TestCancelNotFoundIsStillAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reminder not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "cancel", "doesnotexist"); err == nil {
		t.Error(`runCLI("cancel" on unknown id) error = nil, want an error -- 404 is only an empty state for next/last`)
	}
}

func TestArchiveLimit(t *testing.T) {
	full := []reminder.ArchivedReminder{
		{Reminder: reminder.Reminder{ID: "oldest"}},
		{Reminder: reminder.Reminder{ID: "middle"}},
		{Reminder: reminder.Reminder{ID: "newest"}},
	}
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Total-Count", "3")
		result := full
		if gotQuery == "limit=2" {
			result = full[1:]
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "archive"); err != nil {
		t.Fatalf(`runCLI("archive") error = %v`, err)
	}

	if gotQuery != "limit=20" {
		t.Errorf("default archive query = %q, want %q (the CLI's own --limit default)", gotQuery, "limit=20")
	}

	if strings.Contains(out.String(), "--limit") {
		t.Errorf("archive under the limit printed a truncation hint: %q", out.String())
	}

	out.Reset()
	if err := runCLI(t, a, "archive", "--limit=2"); err != nil {
		t.Fatalf(`runCLI("archive --limit=2") error = %v`, err)
	}

	if gotQuery != "limit=2" {
		t.Errorf("archive --limit=2 query = %q, want %q", gotQuery, "limit=2")
	}
	got := out.String()

	if strings.Contains(got, "oldest") {
		t.Errorf("--limit=2 output still contains the oldest entry: %q", got)
	}

	if !strings.Contains(got, "middle") || !strings.Contains(got, "newest") {
		t.Errorf("--limit=2 output = %q, want the two most recent entries", got)
	}

	if !strings.Contains(got, "showing 2 of 3") {
		t.Errorf("--limit=2 output = %q, want the truncation hint built from X-Total-Count", got)
	}
}

func TestArchiveVerbose(t *testing.T) {
	archived := []reminder.ArchivedReminder{
		{
			Reminder: reminder.Reminder{
				ID:             "abc123",
				Text:           "renew certificate",
				OutboundTopics: []string{"ops-alerts", "backup-topic"},
			},
			NtfyMessageIDs: map[string]string{
				"ops-alerts": "01H_OPS_ID",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "1")
		_ = json.NewEncoder(w).Encode(archived)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "archive"); err != nil {
		t.Fatalf(`runCLI("archive") error = %v`, err)
	}
	if strings.Contains(out.String(), "ops-alerts") {
		t.Errorf("non-verbose archive output = %q, want no topic detail", out.String())
	}

	out.Reset()
	if err := runCLI(t, a, "archive", "--verbose"); err != nil {
		t.Fatalf(`runCLI("archive --verbose") error = %v`, err)
	}
	got := out.String()

	if !strings.Contains(got, "├── sent to ops-alerts (ntfy id: 01H_OPS_ID)") {
		t.Errorf("archive --verbose output = %q, want a branch line for ops-alerts with its ntfy id", got)
	}
	if !strings.Contains(got, "└── sent to backup-topic (ntfy id: unknown)") {
		t.Errorf("archive --verbose output = %q, want a final branch line for backup-topic with unknown ntfy id", got)
	}
}

func TestArchiveVerboseIsNoOpUnderJSON(t *testing.T) {
	archived := []reminder.ArchivedReminder{
		{
			Reminder: reminder.Reminder{ID: "abc123", OutboundTopics: []string{"ops-alerts"}},
			NtfyMessageIDs: map[string]string{
				"ops-alerts": "01H_OPS_ID",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "1")
		_ = json.NewEncoder(w).Encode(archived)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "--json", "archive", "--verbose"); err != nil {
		t.Fatalf(`runCLI("--json archive --verbose") error = %v`, err)
	}

	var got []reminder.ArchivedReminder
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf(`"--json archive --verbose" output is not valid JSON: %v; output: %q`, err, out.String())
	}
	if len(got) != 1 || got[0].NtfyMessageIDs["ops-alerts"] != "01H_OPS_ID" {
		t.Errorf(`"--json archive --verbose" decoded to %+v, want the full archived reminder unaffected by --verbose`, got)
	}
}

func TestSearchArchiveVerbose(t *testing.T) {
	archived := []reminder.ArchivedReminder{
		{
			Reminder: reminder.Reminder{
				ID:             "archived-1",
				Text:           "buy milk",
				OutboundTopics: []string{"family"},
			},
			NtfyMessageIDs: map[string]string{"family": "01H_FAM_ID"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(archived)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "search", "milk", "--archive", "--verbose"); err != nil {
		t.Fatalf(`runCLI("search milk --archive --verbose") error = %v`, err)
	}

	if want := "└── sent to family (ntfy id: 01H_FAM_ID)"; !strings.Contains(out.String(), want) {
		t.Errorf("search --archive --verbose output = %q, want it to contain %q", out.String(), want)
	}
}

func TestSearch(t *testing.T) {
	pending := []reminder.Reminder{{ID: "pending-1", Text: "buy milk"}}
	archived := []reminder.ArchivedReminder{{Reminder: reminder.Reminder{ID: "archived-1", Text: "buy milk"}}}

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if r.URL.Path == "/reminders/archive" {
			_ = json.NewEncoder(w).Encode(archived)
			return
		}
		_ = json.NewEncoder(w).Encode(pending)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "search", "buy", "milk"); err != nil {
		t.Fatalf(`runCLI("search buy milk") error = %v`, err)
	}

	if gotPath != "/reminders" {
		t.Errorf("default search path = %q, want %q (pending)", gotPath, "/reminders")
	}

	if gotQuery != "q=buy+milk" {
		t.Errorf("search query = %q, want %q", gotQuery, "q=buy+milk")
	}

	if !strings.Contains(out.String(), "pending-1") {
		t.Errorf("search output = %q, want it to contain the pending result", out.String())
	}

	out.Reset()
	if err := runCLI(t, a, "search", "milk", "--archive"); err != nil {
		t.Fatalf(`runCLI("search milk --archive") error = %v`, err)
	}

	if gotPath != "/reminders/archive" {
		t.Errorf("--archive search path = %q, want %q", gotPath, "/reminders/archive")
	}

	if !strings.Contains(out.String(), "archived-1") {
		t.Errorf("--archive search output = %q, want it to contain the archived result", out.String())
	}

	if err := runCLI(t, a, "search", "milk", "--pending", "--archive"); err == nil {
		t.Error(`runCLI("search milk --pending --archive") error = nil, want the xor validation to reject both flags together`)
	}
}

func TestSearch_NoMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]reminder.Reminder{})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "search", "nonexistent"); err != nil {
		t.Fatalf(`runCLI("search nonexistent") error = %v`, err)
	}
	if got := out.String(); got != "no pending reminders match\n" {
		t.Errorf("no-match output = %q, want the no-match message", got)
	}
}

func TestJSONOutput(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	pending := []reminder.Reminder{{ID: "abc123", Text: "buy milk"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pending[0])
		default:
			_ = json.NewEncoder(w).Encode(pending)
		}
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "--json", "list"); err != nil {
		t.Fatalf(`runCLI("--json list") error = %v`, err)
	}
	var gotList []reminder.Reminder

	if err := json.Unmarshal(out.Bytes(), &gotList); err != nil {
		t.Fatalf(`"--json list" output is not valid JSON: %v; output: %q`, err, out.String())
	}

	if len(gotList) != 1 || gotList[0].ID != "abc123" {
		t.Errorf(`"--json list" decoded to %+v, want the server's list`, gotList)
	}

	out.Reset()
	if err := runCLI(t, a, "--json", "in", "3", "days", "buy", "milk"); err != nil {
		t.Fatalf(`runCLI("--json <free text>") error = %v`, err)
	}
	var gotRem reminder.Reminder

	if err := json.Unmarshal(out.Bytes(), &gotRem); err != nil {
		t.Fatalf(`"--json" create output is not valid JSON: %v; output: %q`, err, out.String())
	}

	if gotRem.ID != "abc123" {
		t.Errorf(`"--json" create decoded to %+v, want the created reminder`, gotRem)
	}
}

func TestTestParse(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/test/parse" {
			t.Errorf("request = %s %s, want POST /test/parse", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_ = json.NewEncoder(w).Encode(struct {
			Text  string    `json:"text"`
			DueAt time.Time `json:"due_at"`
		}{Text: "go to bed", DueAt: time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC)})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "test", "parse", "tomorrow", "at", "2am", "go", "to", "bed"); err != nil {
		t.Fatalf("runCLI() error = %v", err)
	}

	if gotBody != `{"text":"tomorrow at 2am go to bed"}` {
		t.Errorf("server received body %s, want the args joined into text", gotBody)
	}

	if !strings.Contains(out.String(), "go to bed") {
		t.Errorf("output = %q, want it to contain the previewed task text", out.String())
	}
}

func TestTestParse_JSON(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Text  string    `json:"text"`
			DueAt time.Time `json:"due_at"`
		}{Text: "go to bed", DueAt: time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC)})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "test", "parse", "tomorrow", "at", "2am", "go", "to", "bed", "--json"); err != nil {
		t.Fatalf(`runCLI(..., "--json") error = %v`, err)
	}

	var got struct {
		Text  string    `json:"text"`
		DueAt time.Time `json:"due_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v; output: %q", err, out.String())
	}
	if got.Text != "go to bed" {
		t.Errorf("--json output = %+v, want Text = %q", got, "go to bed")
	}
}

func TestTestParse_ServerRejectionSurfacesRealReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid input: no time information found in: \"buy eggs\""}`))
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	a := &app{out: &out, url: srv.URL, token: "tk_test"}

	err := runCLI(t, a, "test", "parse", "buy", "eggs")
	if err == nil {
		t.Fatal("runCLI() error = nil, want the server's rejection reason surfaced")
	}
	if !strings.Contains(err.Error(), "no time information found") {
		t.Errorf("error = %q, want the real parse failure reason", err)
	}
}

func TestCreateWithTopics(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}
	want := `{"text":"in 3 days standup","outbound_topics":["work","alerts"]}`

	if err := runCLI(t, a, "--topic=work", "--topic=alerts", "in", "3", "days", "standup"); err != nil {
		t.Fatalf("runCLI() with leading --topic error = %v", err)
	}

	if gotBody != want {
		t.Errorf("leading --topic: server received body %s, want %s", gotBody, want)
	}

	gotBody = ""
	if err := runCLI(t, a, "in", "3", "days", "standup", "--topic=work,alerts"); err != nil {
		t.Fatalf("runCLI() with trailing --topic error = %v", err)
	}

	if gotBody != want {
		t.Errorf("trailing --topic: server received body %s, want %s", gotBody, want)
	}
}

func TestCreateWithTags(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "buy", "cake", "in", "3", "days", "--tag=partying_face,birthday"); err != nil {
		t.Fatalf("runCLI() with --tag error = %v", err)
	}

	if want := `{"text":"buy cake in 3 days","tags":["partying_face","birthday"]}`; gotBody != want {
		t.Errorf("server received body %s, want %s", gotBody, want)
	}

	gotBody = ""

	if err := runCLI(t, a, "buy", "cake", "in", "3", "days", "--topic=family", "--tag=birthday"); err != nil {
		t.Fatalf("runCLI() with --topic and --tag error = %v", err)
	}

	if want := `{"text":"buy cake in 3 days","outbound_topics":["family"],"tags":["birthday"]}`; gotBody != want {
		t.Errorf("server received body %s, want %s", gotBody, want)
	}
}

func TestCreateWithPriority(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "go", "to", "airport", "in", "8", "hours", "--priority=urgent"); err != nil {
		t.Fatalf("runCLI() with --priority error = %v", err)
	}

	if want := `{"text":"go to airport in 8 hours","priority":"urgent"}`; gotBody != want {
		t.Errorf("server received body %s, want %s", gotBody, want)
	}

	gotBody = ""

	if err := runCLI(t, a, "go", "to", "airport", "in", "8", "hours", "--priority=hgih"); err == nil {
		t.Fatal("runCLI() with an invalid --priority succeeded, want a kong enum parse error")
	}

	if gotBody != "" {
		t.Errorf("server was reached with body %s, want no request for an invalid priority", gotBody)
	}
}

func TestCreateWithClick(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{out: io.Discard, url: srv.URL, token: "tk_test"}

	if err := runCLI(t, a, "pay", "invoice", "tomorrow", "--click=https://example.com/invoice"); err != nil {
		t.Fatalf("runCLI() with --click error = %v", err)
	}

	if want := `{"text":"pay invoice tomorrow","click":"https://example.com/invoice"}`; gotBody != want {
		t.Errorf("server received body %s, want %s", gotBody, want)
	}
}

func TestPipedStdinCreates(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reminder.Reminder{ID: "abc123"})
	}))
	t.Cleanup(srv.Close)

	a := &app{
		out:   io.Discard,
		url:   srv.URL,
		token: "tk_test",
		stdin: strings.NewReader("in 3d call xyz\n"),
	}

	if err := runCLI(t, a); err != nil {
		t.Fatalf("runCLI() with piped stdin error = %v", err)
	}

	if gotBody != `{"text":"in 3d call xyz"}` {
		t.Errorf("server received body %s, want the trimmed stdin content as text", gotBody)
	}
}

func TestNoTextAndNoStdinErrors(t *testing.T) {
	a := &app{out: io.Discard, url: "http://irrelevant", token: "tk_test", stdin: nil}
	err := runCLI(t, a)

	if err == nil {
		t.Fatal("runCLI() with no text and no pipe error = nil, want an error")
	}

	if !strings.Contains(err.Error(), "stdin") && !strings.Contains(err.Error(), "pipe") {
		t.Errorf("error = %q, want it to mention the piping alternative", err)
	}
}

func TestSaveAndResolveLastID(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, err := resolveID("last"); err == nil {
		t.Error(`resolveID("last") error = nil before any save, want an error`)
	}

	if id, err := resolveID("abc123"); err != nil || id != "abc123" {
		t.Errorf(`resolveID("abc123") = %q, %v; want passthrough`, id, err)
	}

	saveLastID("def456")
	id, err := resolveID("last")
	if err != nil {
		t.Fatalf(`resolveID("last") error = %v after save`, err)
	}
	if id != "def456" {
		t.Errorf(`resolveID("last") = %q, want %q`, id, "def456")
	}
}
