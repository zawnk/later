package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONError(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(rr, "reminder not found", http.StatusNotFound)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}

	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		Error string `json:"error"`
	}

	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error body: %v (body: %s)", err, rr.Body.String())
	}

	if body.Error != "reminder not found" {
		t.Errorf("error field = %q, want %q", body.Error, "reminder not found")
	}
}
