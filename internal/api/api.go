package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zawnk/later/internal/actiontoken"
	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/service"
)

type NotificationClearer interface {
	Clear(ctx context.Context, topicIDs map[string]string) error
}

type API struct {
	cfg              *config.Config
	svc              *service.Service
	actionSecret     []byte
	clearer          NotificationClearer
	actionTokensUsed *actiontoken.UsedTokenTracker
}

func New(cfg *config.Config, svc *service.Service, actionSecret []byte, clearer NotificationClearer) *API {
	return &API{cfg: cfg, svc: svc, actionSecret: actionSecret, clearer: clearer, actionTokensUsed: actiontoken.NewUsedTracker()}
}

func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reminders", a.auth(a.createReminder, ""))
	mux.HandleFunc("GET /reminders", a.auth(a.listPending, ""))
	mux.HandleFunc("GET /reminders/archive", a.auth(a.listArchive, ""))
	mux.HandleFunc("GET /reminders/next", a.auth(a.nextReminder, ""))
	mux.HandleFunc("GET /reminders/last", a.auth(a.lastReminder, ""))
	mux.HandleFunc("GET /reminders/{id}", a.auth(a.getReminder, ""))
	mux.HandleFunc("DELETE /reminders/{id}", a.auth(a.cancelReminder, ""))
	mux.HandleFunc("POST /reminders/{id}/postpone", a.auth(a.postponeReminder, "postpone"))
	mux.HandleFunc("POST /reminders/{id}/dismiss", a.auth(a.dismissReminder, "clear"))
	mux.HandleFunc("POST /test/parse", a.auth(a.testParse, ""))
	mux.HandleFunc("GET /healthz", a.healthz)

	return mux
}

// auth authenticates a request against the configured auth_tokens list.
// actionTokenScope, when non-empty, additionally accepts a scoped jwt
// token (see internal/actiontoken) for exactly that action on this
// route - the fallback a fired notification's action button uses. Empty
// for every route except postpone: a token scoped to "postpone" only
// ever verifies against actionTokenScope == "postpone", so it can't
// authenticate anywhere else no matter how many routes share this one
// function, since each route declares its own expected scope explicitly.
// An action token is also single-use (via actionTokensUsed) - both of a
// reminder's action buttons share one minted token, and the same
// notification can land on more than one subscribed device, so without
// this a second tap (another button, another device) would silently
// postpone the same reminder twice.
func (a *API) auth(next http.HandlerFunc, actionTokenScope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeJSONError(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if verifiedToken, authorized := a.tokenCompare(presented); authorized {
			r = r.WithContext(contextWithToken(r.Context(), verifiedToken))
			next(w, r)
			return
		}

		if actionTokenScope != "" {
			if claims, err := actiontoken.Verify(a.actionSecret, presented, r.PathValue("id"), actionTokenScope); err == nil {
				if !a.actionTokensUsed.MarkUsed(claims.ID, claims.ExpiresAt.Time) {
					writeJSONError(w, "action token already used", http.StatusUnauthorized)
					return
				}
				next(w, r)
				return
			}
		}

		writeJSONError(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (a *API) tokenCompare(presented string) (config.Token, bool) {
	if presented == "" {
		return config.Token{}, false
	}

	presentedBytes := []byte(presented)
	for _, t := range a.cfg.AuthTokens {
		stored := []byte(t.Token)
		if subtle.ConstantTimeCompare(stored, presentedBytes) == 1 {
			return t, true
		}
	}
	return config.Token{}, false
}

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) createReminder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text           string   `json:"text"`
		OutboundTopics []string `json:"outbound_topics"`
		Tags           []string `json:"tags"`
		Priority       string   `json:"priority"`
		Click          string   `json:"click"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Text == "" {
		writeJSONError(w, "text is required", http.StatusBadRequest)
		return
	}

	token := tokenFromContext(r.Context())
	outbound := body.OutboundTopics
	if len(outbound) == 0 {
		if token.DefaultOutbound != "" {
			outbound = []string{token.DefaultOutbound}
		} else {
			outbound = slices.Clone(token.Outbound)
		}
	} else {
		outbound = filterAllowed(outbound, token.Outbound)
		if len(outbound) == 0 {
			writeJSONError(w, "no allowed outbound topics", http.StatusForbidden)
			return
		}
	}

	rem, err := a.svc.CreateReminder(service.CreateInput{
		Text:           body.Text,
		OutboundTopics: outbound,
		Tags:           body.Tags,
		Priority:       body.Priority,
		Click:          body.Click,
	})
	if err != nil {
		writeServiceError(w, err, "failed to create reminder")
		return
	}

	writeJsonResponse(w, http.StatusCreated, rem)
}

func (a *API) listPending(w http.ResponseWriter, r *http.Request) {
	reminders := a.svc.ListPending()

	if r.URL.Query().Get("q") != "" {
		reminders = filterByQuery(reminders, r.URL.Query().Get("q"), func(r reminder.Reminder) string { return r.Text })
	}

	switch sortBy := r.URL.Query().Get("sort"); sortBy {
	case "", "due":
		slices.SortFunc(reminders, func(x, y reminder.Reminder) int { return x.DueAt.Compare(y.DueAt) })
	case "create":
		slices.SortFunc(reminders, func(x, y reminder.Reminder) int { return x.CreatedAt.Compare(y.CreatedAt) })
	default:
		writeJSONError(w, "sort must be 'due' or 'create'", http.StatusBadRequest)
		return
	}

	writeJsonResponse(w, http.StatusOK, reminders)
}

func (a *API) listArchive(w http.ResponseWriter, r *http.Request) {
	reminders, err := a.svc.ListArchive()
	if err != nil {
		writeJSONError(w, "failed to load archive", http.StatusInternalServerError)
		return
	}
	reminders = filterByQuery(reminders, r.URL.Query().Get("q"), func(r reminder.ArchivedReminder) string { return r.Text })
	total := len(reminders)

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)

		if err != nil || limit < 0 {
			writeJSONError(w, "limit must be a non-negative integer", http.StatusBadRequest)
			return
		}

		if limit > 0 && total > limit {
			reminders = reminders[total-limit:]
		}
	}

	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJsonResponse(w, http.StatusOK, reminders)
}

func filterByQuery[T any](items []T, q string, text func(T) string) []T {
	if q == "" {
		return items
	}
	q = strings.ToLower(q)
	filtered := make([]T, 0, len(items))
	for _, item := range items {
		if strings.Contains(strings.ToLower(text(item)), q) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (a *API) getReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pending, archived, err := a.svc.Get(id)

	if errors.Is(err, service.ErrNotFound) {
		writeJSONError(w, "reminder not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSONError(w, "failed to load reminder", http.StatusInternalServerError)
		return
	}

	if pending != nil {
		writeJsonResponse(w, http.StatusOK, pending)
		return
	}
	writeJsonResponse(w, http.StatusOK, archived)
}

func (a *API) nextReminder(w http.ResponseWriter, r *http.Request) {
	rem := a.svc.Next()
	if rem == nil {
		writeJSONError(w, "no pending reminders", http.StatusNotFound)
		return
	}
	writeJsonResponse(w, http.StatusOK, rem)
}

func (a *API) lastReminder(w http.ResponseWriter, r *http.Request) {
	rem, err := a.svc.Last()
	if err != nil {
		writeJSONError(w, "failed to load archive", http.StatusInternalServerError)
		return
	}
	if rem == nil {
		writeJSONError(w, "no archived reminders", http.StatusNotFound)
		return
	}
	writeJsonResponse(w, http.StatusOK, rem)
}

func (a *API) cancelReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found, err := a.svc.Cancel(id)
	if err != nil {
		writeJSONError(w, "failed to cancel reminder", http.StatusInternalServerError)
		return
	}
	if !found {
		writeJSONError(w, "reminder not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postponeReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	duration := r.URL.Query().Get("duration")
	if duration == "" {
		writeJSONError(w, "duration is required", http.StatusBadRequest)
		return
	}

	rem, err := a.svc.Postpone(id, duration)
	if err != nil {
		writeServiceError(w, err, "failed to postpone reminder")
		return
	}
	writeJsonResponse(w, http.StatusCreated, rem)
}

func (a *API) dismissReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	archived, err := a.svc.GetArchived(id)
	if err != nil {
		writeServiceError(w, err, "failed to load archived reminder")
		return
	}

	if err := a.clearer.Clear(r.Context(), archived.NtfyMessageIDs); err != nil {
		slog.Error("failed to clear notification", "id", id, "err", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// testParse previews how text would be parsed by CreateReminder - task
// text and resolved due time - without creating or storing anything.
func (a *API) testParse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	task, due, err := a.svc.ParseReminderText(body.Text)
	if err != nil {
		writeServiceError(w, err, "failed to parse")
		return
	}

	writeJsonResponse(w, http.StatusOK, struct {
		Text  string    `json:"text"`
		DueAt time.Time `json:"due_at"`
	}{Text: task, DueAt: due})
}

func filterAllowed(requested, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, t := range allowed {
		allowedSet[t] = struct{}{}
	}
	var result []string
	for _, t := range requested {
		if _, ok := allowedSet[t]; ok {
			result = append(result, t)
		}
	}
	return reminder.DedupeStrings(result)
}
