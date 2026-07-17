package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

func writeJsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to write JSON response", "err", err)
	}
}
