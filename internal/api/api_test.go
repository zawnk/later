package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zawnk/later/internal/actiontoken"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/service"
)

var testActionSecret = []byte("test-action-secret")

func TestFilterAllowed(t *testing.T) {
	tests := []struct {
		name      string
		requested []string
		allowed   []string
		grant     []string
	}{
		{
			name:      "all requested topics are allowed",
			requested: []string{"a", "b"},
			allowed:   []string{"a", "b", "c"},
			grant:     []string{"a", "b"},
		},
		{
			name:      "some requested topics are not allowed",
			requested: []string{"a", "z"},
			allowed:   []string{"a", "b"},
			grant:     []string{"a"},
		},
		{
			name:      "none of the requested topics are allowed",
			requested: []string{"z"},
			allowed:   []string{"a", "b"},
			grant:     nil,
		},
		{
			name:      "empty requested list",
			requested: []string{},
			allowed:   []string{"a"},
			grant:     nil,
		},
		{
			name:      "same requested multiple times",
			requested: []string{"a", "a"},
			allowed:   []string{"a"},
			grant:     []string{"a"},
		},
		{
			name:      "empty allow list",
			requested: []string{"a"},
			allowed:   []string{},
			grant:     nil,
		},
		{
			name:      "requested order is preserved, not alphabetized",
			requested: []string{"b", "a"},
			allowed:   []string{"a", "b"},
			grant:     []string{"b", "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterAllowed(tt.requested, tt.allowed)
			if !reflect.DeepEqual(got, tt.grant) {
				t.Errorf("filterAllowed(%v, %v) = %v, grant %v", tt.requested, tt.allowed, got, tt.grant)
			}
		})
	}
}

func TestTokenCompare(t *testing.T) {
	a := &API{cfg: &config.Config{
		AuthTokens: []config.Token{
			{Token: "token-one", Outbound: []string{"topic-a"}},
			{Token: "token-two", Outbound: []string{"topic-b"}},
		},
	}}

	tests := []struct {
		name      string
		presented string
		wantOK    bool
		wantIdx   int
	}{
		{"empty presented token is rejected", "", false, -1},
		{"unknown token is rejected", "not-a-real-token", false, -1},
		{"first configured token matches", "token-one", true, 0},
		{"second configured token matches", "token-two", true, 1},
		{"prefix of a real token does not match", "token-on", false, -1},
		{"real token plus extra suffix does not match", "token-one-extra", false, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := a.tokenCompare(tt.presented)
			if ok != tt.wantOK {
				t.Fatalf("tokenCompare(%q) ok = %v, want %v", tt.presented, ok, tt.wantOK)
			}

			if !tt.wantOK {
				return
			}

			want := a.cfg.AuthTokens[tt.wantIdx]
			if got.Token != want.Token {
				t.Errorf("tokenCompare(%q) matched token %q, want %q", tt.presented, got.Token, want.Token)
			}
		})
	}
}

func TestTokenCompare_NoTokensConfigured(t *testing.T) {
	a := &API{cfg: &config.Config{}}
	if _, ok := a.tokenCompare("anything"); ok {
		t.Error("tokenCompare() ok = true, want false when no auth_tokens are configured")
	}
}

func TestAuth(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{
			{Token: "valid-token", Outbound: []string{"topic-a"}},
		},
	}
	a := New(cfg, nil, testActionSecret, nil)

	var (
		called          bool
		calledWithToken config.Token
	)
	next := func(w http.ResponseWriter, r *http.Request) {
		called = true
		calledWithToken = tokenFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
		wantCalled bool
	}{
		{"missing Authorization header is rejected", "", http.StatusUnauthorized, false},
		{"wrong token is rejected", "Bearer wrong-token", http.StatusUnauthorized, false},
		{"correct token is accepted", "Bearer valid-token", http.StatusOK, true},
		{"Bearer prefix with empty token is rejected", "Bearer ", http.StatusUnauthorized, false},
		{"header without the Bearer prefix is treated as a literal (wrong) token", "NotBearer valid-token", http.StatusUnauthorized, false},
		{"bare valid token without the Bearer scheme is rejected", "valid-token", http.StatusUnauthorized, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called = false
			calledWithToken = config.Token{}

			req := httptest.NewRequest(http.MethodGet, "/reminders", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rr := httptest.NewRecorder()

			a.auth(next, "")(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tt.wantStatus)
			}

			if called != tt.wantCalled {
				t.Errorf("next called = %v, want %v", called, tt.wantCalled)
			}

			if tt.wantCalled && calledWithToken.Token != "valid-token" {
				t.Errorf("next saw token %q in request context, want %q", calledWithToken.Token, "valid-token")
			}
		})
	}
}

type stubStore struct {
	saved   []reminder.Reminder
	pending []reminder.Reminder
	archive []reminder.ArchivedReminder
}

func (s *stubStore) SaveReminder(r reminder.Reminder) error    { s.saved = append(s.saved, r); return nil }
func (s *stubStore) ListPendingReminders() []reminder.Reminder { return s.pending }
func (s *stubStore) ListArchive() ([]reminder.ArchivedReminder, error) {
	return s.archive, nil
}
func (s *stubStore) CancelReminder(id string) (bool, error) { return false, nil }

func TestCreateReminder_TopicScoping(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{
			{Token: "valid-token", Outbound: []string{"topic-a", "topic-b"}},
			{Token: "token-with-default", Outbound: []string{"topic-a", "topic-b"}, DefaultOutbound: "topic-b"},
		},
	}

	tests := []struct {
		name       string
		token      string
		body       string
		wantStatus int
		wantTopics []string
	}{
		{
			name:       "no default, no topics: all the token's topics",
			token:      "valid-token",
			body:       `{"text":"buy milk in 3 days"}`,
			wantStatus: http.StatusCreated,
			wantTopics: []string{"topic-a", "topic-b"},
		},
		{
			name:       "no default, allowed explicit topic narrows the list",
			token:      "valid-token",
			body:       `{"text":"buy milk in 3 days","outbound_topics":["topic-b"]}`,
			wantStatus: http.StatusCreated,
			wantTopics: []string{"topic-b"},
		},
		{
			name:       "default set, no topics: the default only",
			token:      "token-with-default",
			body:       `{"text":"buy milk in 3 days"}`,
			wantStatus: http.StatusCreated,
			wantTopics: []string{"topic-b"},
		},
		{
			name:       "default set, explicit topic overrides the default",
			token:      "token-with-default",
			body:       `{"text":"buy milk in 3 days","outbound_topics":["topic-a"]}`,
			wantStatus: http.StatusCreated,
			wantTopics: []string{"topic-a"},
		},
		{
			name:       "disallowed topics are dropped, allowed ones survive",
			token:      "valid-token",
			body:       `{"text":"buy milk in 3 days","outbound_topics":["topic-b","not-my-topic"]}`,
			wantStatus: http.StatusCreated,
			wantTopics: []string{"topic-b"},
		},
		{
			name:       "only disallowed topics is a 403, nothing stored",
			token:      "valid-token",
			body:       `{"text":"buy milk in 3 days","outbound_topics":["not-my-topic"]}`,
			wantStatus: http.StatusForbidden,
			wantTopics: nil,
		},
		{
			name:       "unknown field in body is rejected",
			token:      "valid-token",
			body:       `{"text":"buy milk in 3 days","topic":"topic-a"}`,
			wantStatus: http.StatusBadRequest,
			wantTopics: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodPost, "/reminders", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+tt.token)
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantTopics == nil {
				if len(store.saved) != 0 {
					t.Fatalf("store has %d reminders after a rejected request, want 0", len(store.saved))
				}
				return
			}

			var rem reminder.Reminder
			if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
				t.Fatalf("decoding response: %v", err)
			}

			if !slices.Equal(rem.OutboundTopics, tt.wantTopics) {
				t.Errorf("response OutboundTopics = %v, want %v", rem.OutboundTopics, tt.wantTopics)
			}

			if len(store.saved) != 1 {
				t.Fatalf("store has %d reminders, want 1", len(store.saved))
			}

			if !slices.Equal(store.saved[0].OutboundTopics, tt.wantTopics) {
				t.Errorf("stored OutboundTopics = %v, want %v", store.saved[0].OutboundTopics, tt.wantTopics)
			}
		})
	}
}

func TestListArchive_Pagination(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	archive := []reminder.ArchivedReminder{
		{Reminder: reminder.Reminder{ID: "1", Text: "one"}},
		{Reminder: reminder.Reminder{ID: "2", Text: "two"}},
		{Reminder: reminder.Reminder{ID: "3", Text: "three"}},
	}

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantIDs    []string
		wantTotal  string
	}{
		{"no limit param returns everything", "", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"limit narrower than total returns the most recent N, oldest-first", "?limit=2", http.StatusOK, []string{"2", "3"}, "3"},
		{"limit wider than total returns everything", "?limit=10", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"limit=0 means no limit", "?limit=0", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"negative limit is rejected", "?limit=-1", http.StatusBadRequest, nil, ""},
		{"non-numeric limit is rejected", "?limit=abc", http.StatusBadRequest, nil, ""},
		{"offset alone skips the N most recent", "?offset=1", http.StatusOK, []string{"1", "2"}, "3"},
		{"limit and offset page backwards from the most recent", "?limit=1&offset=1", http.StatusOK, []string{"2"}, "3"},
		{"the last page is the oldest entry", "?limit=1&offset=2", http.StatusOK, []string{"1"}, "3"},
		{"a page past the oldest entry is empty", "?limit=1&offset=3", http.StatusOK, []string{}, "3"},
		{"offset past the total is empty, not an error", "?offset=99", http.StatusOK, []string{}, "3"},
		{"a page overlapping the start is clamped, not wrapped", "?limit=2&offset=2", http.StatusOK, []string{"1"}, "3"},
		{"offset=0 is the first page", "?limit=2&offset=0", http.StatusOK, []string{"2", "3"}, "3"},
		{"negative offset is rejected", "?offset=-1", http.StatusBadRequest, nil, ""},
		{"non-numeric offset is rejected", "?offset=abc", http.StatusBadRequest, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{archive: archive}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodGet, "/reminders/archive"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantIDs == nil {
				return
			}

			if got := rr.Header().Get("X-Total-Count"); got != tt.wantTotal {
				t.Errorf("X-Total-Count = %q, want %q (the true total, unaffected by limit)", got, tt.wantTotal)
			}

			var got []reminder.ArchivedReminder

			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			var gotIDs []string
			for _, r := range got {
				gotIDs = append(gotIDs, r.ID)
			}

			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("archive IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListArchive_Search(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	archive := []reminder.ArchivedReminder{
		{Reminder: reminder.Reminder{ID: "1", Text: "buy milk"}},
		{Reminder: reminder.Reminder{ID: "2", Text: "call the plumber"}},
		{Reminder: reminder.Reminder{ID: "3", Text: "Buy Bread"}},
	}

	tests := []struct {
		name      string
		query     string
		wantIDs   []string
		wantTotal string
	}{
		{"no q param returns everything", "", []string{"1", "2", "3"}, "3"},
		{"substring match", "?q=milk", []string{"1"}, "1"},
		{"case-insensitive match", "?q=BUY", []string{"1", "3"}, "2"},
		{"no matches", "?q=nonexistent", []string{}, "0"},
		{"q and limit combine: filters first, then truncates the filtered set", "?q=buy&limit=1", []string{"3"}, "2"},
		{"offset pages within the filtered set, not the whole archive", "?q=buy&limit=1&offset=1", []string{"1"}, "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{archive: slices.Clone(archive)}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodGet, "/reminders/archive"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
			}

			if got := rr.Header().Get("X-Total-Count"); got != tt.wantTotal {
				t.Errorf("X-Total-Count = %q, want %q", got, tt.wantTotal)
			}

			var got []reminder.ArchivedReminder
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			gotIDs := []string{}
			for _, r := range got {
				gotIDs = append(gotIDs, r.ID)
			}

			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("archive IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestPostponeReminder(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	archived := reminder.ArchivedReminder{
		Reminder: reminder.Reminder{ID: "abc123", Text: "buy milk", OutboundTopics: []string{"topic-a"}},
	}

	newStore := func() *stubStore { return &stubStore{archive: []reminder.ArchivedReminder{archived}} }

	t.Run("real token: unchanged behavior", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=1d", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusCreated, rr.Body.String())
		}
	})

	t.Run("real token without duration is a 400", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
		}
	})

	t.Run("no Authorization header is a 401", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=1d", nil)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("garbage bearer value (neither a real token nor a valid action token) is a 401", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=1d", nil)
		req.Header.Set("Authorization", "Bearer complete-nonsense")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("valid jwt token for this reminder succeeds, duration accepts a natural-language phrase", func(t *testing.T) {
		store := newStore()
		a := New(cfg, service.New(store), testActionSecret, nil)

		token, err := actiontoken.Mint(testActionSecret, "abc123", "postpone")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=in+1h", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusCreated, rr.Body.String())
		}

		var rem reminder.Reminder
		if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if rem.Text != "buy milk" {
			t.Errorf("Text = %q, want %q (carried over from the archived record, not the token)", rem.Text, "buy milk")
		}
	})

	t.Run("a used action token cannot be replayed - e.g. the other button on the same notification, or a second device on the same topic", func(t *testing.T) {
		store := newStore()
		a := New(cfg, service.New(store), testActionSecret, nil)

		token, err := actiontoken.Mint(testActionSecret, "abc123", "postpone")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		first := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=in+1h", nil)
		first.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, first)
		if rr.Code != http.StatusCreated {
			t.Fatalf("first use: status = %d, want %d (body: %s)", rr.Code, http.StatusCreated, rr.Body.String())
		}

		replay := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone?duration=tomorrow+morning", nil)
		replay.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, replay)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("replay: status = %d, want %d (body: %s)", rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	})

	t.Run("action token scoped to a different reminder id is rejected", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, nil)

		token, err := actiontoken.Mint(testActionSecret, "some-other-id", "postpone")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/postpone", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})
}

type stubClearer struct {
	calls   []map[string]string
	err     error
	callErr func(topicIDs map[string]string) error
}

func (c *stubClearer) Clear(ctx context.Context, topicIDs map[string]string) error {
	c.calls = append(c.calls, topicIDs)
	if c.callErr != nil {
		return c.callErr(topicIDs)
	}
	return c.err
}

func TestDismissReminder(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	archived := reminder.ArchivedReminder{
		Reminder:       reminder.Reminder{ID: "abc123", Text: "buy milk", OutboundTopics: []string{"topic-a"}},
		NtfyMessageIDs: map[string]string{"topic-a": "ntfy-id-1"},
	}
	newStore := func() *stubStore { return &stubStore{archive: []reminder.ArchivedReminder{archived}} }

	t.Run("real token: clears every topic in NtfyMessageIDs and returns 204", func(t *testing.T) {
		clearer := &stubClearer{}
		a := New(cfg, service.New(newStore()), testActionSecret, clearer)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusNoContent, rr.Body.String())
		}
		if len(clearer.calls) != 1 {
			t.Fatalf("Clear() called %d times, want 1", len(clearer.calls))
		}
		if clearer.calls[0]["topic-a"] != "ntfy-id-1" {
			t.Errorf("Clear() called with %+v, want topic-a -> ntfy-id-1", clearer.calls[0])
		}
	})

	t.Run("unknown id is a 404, Clear is never called", func(t *testing.T) {
		clearer := &stubClearer{}
		a := New(cfg, service.New(&stubStore{}), testActionSecret, clearer)

		req := httptest.NewRequest(http.MethodPost, "/reminders/no-such-id/dismiss", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
		}
		if len(clearer.calls) != 0 {
			t.Errorf("Clear() called %d times, want 0", len(clearer.calls))
		}
	})

	t.Run("a pending (not yet archived) reminder is a 404 - dismiss is archive-only", func(t *testing.T) {
		clearer := &stubClearer{}
		store := &stubStore{pending: []reminder.Reminder{{ID: "abc123"}}}
		a := New(cfg, service.New(store), testActionSecret, clearer)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
		}
	})

	t.Run("no Authorization header is a 401", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, &stubClearer{})

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("valid clear-scoped jwt token for this reminder succeeds", func(t *testing.T) {
		clearer := &stubClearer{}
		a := New(cfg, service.New(newStore()), testActionSecret, clearer)

		token, err := actiontoken.Mint(testActionSecret, "abc123", "clear")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusNoContent, rr.Body.String())
		}
	})

	t.Run("a postpone-scoped token cannot authenticate dismiss", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, &stubClearer{})

		token, err := actiontoken.Mint(testActionSecret, "abc123", "postpone")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("a used clear action token cannot be replayed", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, &stubClearer{})

		token, err := actiontoken.Mint(testActionSecret, "abc123", "clear")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		first := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		first.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, first)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("first use: status = %d, want %d (body: %s)", rr.Code, http.StatusNoContent, rr.Body.String())
		}

		replay := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		replay.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, replay)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("replay: status = %d, want %d (body: %s)", rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	})

	t.Run("action token scoped to a different reminder id is rejected", func(t *testing.T) {
		a := New(cfg, service.New(newStore()), testActionSecret, &stubClearer{})

		token, err := actiontoken.Mint(testActionSecret, "some-other-id", "clear")
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("Clear() returning an error (every topic failed) still logs but does not fail the response - best-effort", func(t *testing.T) {
		clearer := &stubClearer{err: errors.New("ntfy unreachable")}
		a := New(cfg, service.New(newStore()), testActionSecret, clearer)

		req := httptest.NewRequest(http.MethodPost, "/reminders/abc123/dismiss", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d (body: %s) - a downstream ntfy clear failure shouldn't surface as an API error", rr.Code, http.StatusNoContent, rr.Body.String())
		}
	})
}

func TestTestParse(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}

	postBody := func(text string) io.Reader {
		b, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: text})
		return bytes.NewReader(b)
	}

	t.Run("valid text previews task and due time without persisting anything", func(t *testing.T) {
		store := &stubStore{}
		a := New(cfg, service.New(store), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/test/parse", postBody("buy milk in 3 days"))
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
		}

		var got struct {
			Text  string    `json:"text"`
			DueAt time.Time `json:"due_at"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if got.Text != "buy milk" {
			t.Errorf("Text = %q, want %q", got.Text, "buy milk")
		}
		if len(store.saved) != 0 || len(store.archive) != 0 {
			t.Errorf("store has saved=%d archive=%d, want both 0 (preview must not persist)", len(store.saved), len(store.archive))
		}
	})

	t.Run("unparseable text is a 400 with the real reason", func(t *testing.T) {
		a := New(cfg, service.New(&stubStore{}), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/test/parse", postBody("buy eggs"))
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusBadRequest, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "no time information found") {
			t.Errorf("body = %q, want it to contain the real parse failure reason", rr.Body.String())
		}
	})

	t.Run("unknown field in request body is a 400", func(t *testing.T) {
		a := New(cfg, service.New(&stubStore{}), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/test/parse", strings.NewReader(`{"text":"buy milk in 3 days","topic":"oops"}`))
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
		}
	})

	t.Run("no auth is a 401", func(t *testing.T) {
		a := New(cfg, service.New(&stubStore{}), testActionSecret, nil)

		req := httptest.NewRequest(http.MethodPost, "/test/parse", postBody("buy milk in 3 days"))
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})
}

func TestGetReminder(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	store := &stubStore{
		pending: []reminder.Reminder{{ID: "pending-1", Text: "buy milk"}},
		archive: []reminder.ArchivedReminder{{Reminder: reminder.Reminder{ID: "archived-1", Text: "call mom"}}},
	}
	a := New(cfg, service.New(store), testActionSecret, nil)

	t.Run("finds a pending reminder", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/reminders/pending-1", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
		}
		var rem reminder.Reminder

		if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
			t.Fatalf("decoding response: %v", err)
		}

		if rem.ID != "pending-1" {
			t.Errorf("ID = %q, want %q", rem.ID, "pending-1")
		}
	})

	t.Run("finds an archived reminder", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/reminders/archived-1", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
		}
		var rem reminder.ArchivedReminder

		if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
			t.Fatalf("decoding response: %v", err)
		}

		if rem.ID != "archived-1" {
			t.Errorf("ID = %q, want %q", rem.ID, "archived-1")
		}
	})

	t.Run("unknown id is a 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/reminders/does-not-exist", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
		}
	})

	t.Run("literal routes still take precedence over the id wildcard", func(t *testing.T) {
		now := time.Now()
		collisionStore := &stubStore{
			pending: []reminder.Reminder{
				{ID: "next", DueAt: now.Add(48 * time.Hour)},
				{ID: "real-next", DueAt: now.Add(24 * time.Hour)},
				{ID: "archive", DueAt: now.Add(72 * time.Hour), Text: "collision decoy for /reminders/archive"},
			},
			archive: []reminder.ArchivedReminder{
				{Reminder: reminder.Reminder{ID: "last"}},
				{Reminder: reminder.Reminder{ID: "real-archive-item"}},
				{Reminder: reminder.Reminder{ID: "real-last"}},
			},
		}
		collisionAPI := New(cfg, service.New(collisionStore), testActionSecret, nil)

		get := func(path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			collisionAPI.Routes().ServeHTTP(rr, req)
			return rr
		}

		t.Run("archive returns the list, not a single reminder named 'archive'", func(t *testing.T) {
			rr := get("/reminders/archive")
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
			}
			var got []reminder.ArchivedReminder

			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("response wasn't a JSON array (getReminder would have returned a single object instead): %v", err)
			}
			found := false
			for _, r := range got {
				if r.ID == "real-archive-item" {
					found = true
				}
			}

			if !found {
				t.Errorf("archive listing = %v, want it to contain real-archive-item", got)
			}
		})

		t.Run("next returns the soonest pending reminder, not the one literally named 'next'", func(t *testing.T) {
			rr := get("/reminders/next")

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
			}
			var rem reminder.Reminder

			if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
				t.Fatalf("decoding response: %v", err)
			}

			if rem.ID != "real-next" {
				t.Errorf("GET /reminders/next returned id %q, want %q (got the collision decoy instead of the soonest-due reminder)", rem.ID, "real-next")
			}
		})

		t.Run("last returns the last-appended archived reminder, not the one literally named 'last'", func(t *testing.T) {
			rr := get("/reminders/last")

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
			}
			var rem reminder.ArchivedReminder

			if err := json.NewDecoder(rr.Body).Decode(&rem); err != nil {
				t.Fatalf("decoding response: %v", err)
			}

			if rem.ID != "real-last" {
				t.Errorf("GET /reminders/last returned id %q, want %q (got the collision decoy instead of the last-appended entry)", rem.ID, "real-last")
			}
		})
	})
}

func TestListPending_Sort(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	early := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	pending := []reminder.Reminder{
		{ID: "created-first-due-later", DueAt: late, CreatedAt: early},
		{ID: "created-later-due-first", DueAt: early, CreatedAt: late},
	}

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantFirst  string
	}{
		{"no sort param defaults to due", "", http.StatusOK, "created-later-due-first"},
		{"sort=due", "?sort=due", http.StatusOK, "created-later-due-first"},
		{"sort=create", "?sort=create", http.StatusOK, "created-first-due-later"},
		{"unknown sort value is rejected", "?sort=bogus", http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{pending: slices.Clone(pending)}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodGet, "/reminders"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantFirst == "" {
				return
			}

			var got []reminder.Reminder
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}

			if len(got) == 0 || got[0].ID != tt.wantFirst {
				t.Errorf("first reminder = %+v, want ID %q first", got, tt.wantFirst)
			}
		})
	}
}

func TestListPending_Search(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	pending := []reminder.Reminder{
		{ID: "1", Text: "buy milk"},
		{ID: "2", Text: "call the plumber"},
		{ID: "3", Text: "Buy Bread"},
	}

	tests := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{"no q param returns everything", "", []string{"1", "2", "3"}},
		{"substring match", "?q=milk", []string{"1"}},
		{"case-insensitive match", "?q=BUY", []string{"1", "3"}},
		{"no matches", "?q=nonexistent", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{pending: slices.Clone(pending)}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodGet, "/reminders"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
			}

			var got []reminder.Reminder
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			gotIDs := []string{}
			for _, r := range got {
				gotIDs = append(gotIDs, r.ID)
			}

			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("reminder IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListPending_Pagination(t *testing.T) {
	cfg := &config.Config{
		AuthTokens: []config.Token{{Token: "valid-token", Outbound: []string{"topic-a"}}},
	}
	base := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	pending := []reminder.Reminder{
		{ID: "3", Text: "third", DueAt: base.Add(3 * time.Hour)},
		{ID: "1", Text: "first", DueAt: base.Add(1 * time.Hour)},
		{ID: "2", Text: "second", DueAt: base.Add(2 * time.Hour)},
	}

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantIDs    []string
		wantTotal  string
	}{
		{"no params returns everything, soonest-first", "", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"limit takes the soonest N", "?limit=2", http.StatusOK, []string{"1", "2"}, "3"},
		{"limit=0 means no limit", "?limit=0", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"limit wider than total returns everything", "?limit=10", http.StatusOK, []string{"1", "2", "3"}, "3"},
		{"offset alone skips the N soonest", "?offset=1", http.StatusOK, []string{"2", "3"}, "3"},
		{"limit and offset page forwards", "?limit=1&offset=1", http.StatusOK, []string{"2"}, "3"},
		{"a page overlapping the end is clamped", "?limit=2&offset=2", http.StatusOK, []string{"3"}, "3"},
		{"offset past the total is empty, not an error", "?offset=99", http.StatusOK, []string{}, "3"},
		{"paging applies after the sort", "?sort=create&limit=1", http.StatusOK, []string{"3"}, "3"},
		{"paging applies after the search", "?q=ir&limit=1&offset=1", http.StatusOK, []string{"3"}, "2"},
		{"negative limit is rejected", "?limit=-1", http.StatusBadRequest, nil, ""},
		{"non-numeric offset is rejected", "?offset=abc", http.StatusBadRequest, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{pending: slices.Clone(pending)}
			a := New(cfg, service.New(store), testActionSecret, nil)

			req := httptest.NewRequest(http.MethodGet, "/reminders"+tt.query, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantIDs == nil {
				return
			}

			if got := rr.Header().Get("X-Total-Count"); got != tt.wantTotal {
				t.Errorf("X-Total-Count = %q, want %q (the true total, unaffected by paging)", got, tt.wantTotal)
			}

			var got []reminder.Reminder
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			gotIDs := []string{}
			for _, r := range got {
				gotIDs = append(gotIDs, r.ID)
			}

			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("reminder IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}
