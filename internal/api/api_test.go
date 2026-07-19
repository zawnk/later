package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/service"
)

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
	a := New(cfg, nil)

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

			a.auth(next)(rr, req)

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
	saved []reminder.Reminder
}

func (s *stubStore) SaveReminder(r reminder.Reminder) error            { s.saved = append(s.saved, r); return nil }
func (s *stubStore) ListPendingReminders() []reminder.Reminder         { return nil }
func (s *stubStore) ListArchive() ([]reminder.ArchivedReminder, error) { return nil, nil }
func (s *stubStore) CancelReminder(id string) (bool, error)            { return false, nil }

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{}
			a := New(cfg, service.New(store))

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
