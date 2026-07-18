package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/zawnk/later/internal/config"
	"github.com/zawnk/later/internal/service"
)

type API struct {
	cfg *config.Config
	svc *service.Service
}

func New(cfg *config.Config, svc *service.Service) *API {
	return &API{cfg: cfg, svc: svc}
}

func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reminders", a.auth(a.createReminder))
	mux.HandleFunc("GET /reminders", a.auth(a.listPending))
	mux.HandleFunc("GET /reminders/archive", a.auth(a.listArchive))
	mux.HandleFunc("GET /reminders/next", a.auth(a.nextReminder))
	mux.HandleFunc("GET /reminders/last", a.auth(a.lastReminder))
	mux.HandleFunc("DELETE /reminders/{id}", a.auth(a.cancelReminder))
	mux.HandleFunc("POST /reminders/{id}/postpone", a.auth(a.postponeReminder))
	mux.HandleFunc("GET /healthz", a.healthz)

	return mux
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		verifiedToken, authorized := a.tokenCompare(token)

		if !authorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		r = r.WithContext(contextWithToken(r.Context(), verifiedToken))
		next(w, r)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	token := tokenFromContext(r.Context())
	outbound := body.OutboundTopics
	if len(outbound) == 0 {
		outbound = token.Outbound
	} else {
		outbound = filterAllowed(outbound, token.Outbound)
		if len(outbound) == 0 {
			http.Error(w, "no allowed outbound topics", http.StatusForbidden)
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
	writeJsonResponse(w, http.StatusOK, a.svc.ListPending())
}

func (a *API) listArchive(w http.ResponseWriter, r *http.Request) {
	reminders, err := a.svc.ListArchive()
	if err != nil {
		http.Error(w, "failed to load archive", http.StatusInternalServerError)
		return
	}
	writeJsonResponse(w, http.StatusOK, reminders)
}

func (a *API) nextReminder(w http.ResponseWriter, r *http.Request) {
	rem := a.svc.Next()
	if rem == nil {
		http.Error(w, "no pending reminders", http.StatusNotFound)
		return
	}
	writeJsonResponse(w, http.StatusOK, rem)
}

func (a *API) lastReminder(w http.ResponseWriter, r *http.Request) {
	rem, err := a.svc.Last()
	if err != nil {
		http.Error(w, "failed to load archive", http.StatusInternalServerError)
		return
	}
	if rem == nil {
		http.Error(w, "no archived reminders", http.StatusNotFound)
		return
	}
	writeJsonResponse(w, http.StatusOK, rem)
}

func (a *API) cancelReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found, err := a.svc.Cancel(id)
	if err != nil {
		http.Error(w, "failed to cancel reminder", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "reminder not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postponeReminder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	duration := r.URL.Query().Get("duration")
	if duration == "" {
		http.Error(w, "duration is required", http.StatusBadRequest)
		return
	}

	rem, err := a.svc.Postpone(id, duration)
	if err != nil {
		writeServiceError(w, err, "failed to postpone reminder")
		return
	}

	writeJsonResponse(w, http.StatusCreated, rem)
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
	slices.Sort(result)
	return slices.Compact((result))
}
