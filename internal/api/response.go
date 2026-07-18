package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zawnk/later/internal/service"
)

func writeJsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to write JSON response", "err", err)
	}
}

func writeServiceError(w http.ResponseWriter, err error, logMsg string) {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, service.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, service.ErrStillPending):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		slog.Error(logMsg, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
