package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogRequests(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		handlerStatus int
		wantLogged    bool
	}{
		{"normal path is always logged", "/reminders", http.StatusOK, true},
		{"successful /healthz is suppressed", "/healthz", http.StatusNoContent, false},
		{"failing /healthz is still logged", "/healthz", http.StatusInternalServerError, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := swapDefaultLogger(&buf)
			defer restore()

			next := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.handlerStatus)
			}
			handler := LogRequests(http.HandlerFunc(next))

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rr.Code != tt.handlerStatus {
				t.Errorf("status = %d, want %d", rr.Code, tt.handlerStatus)
			}

			if logged := buf.Len() > 0; logged != tt.wantLogged {
				t.Errorf("logged = %v, want %v (log output: %q)", logged, tt.wantLogged, buf.String())
			}
		})
	}
}

func TestLogRequests_StatusRecorder(t *testing.T) {
	t.Run("implicit 200 via Write with no prior WriteHeader", func(t *testing.T) {
		var buf bytes.Buffer
		restore := swapDefaultLogger(&buf)
		defer restore()

		next := func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("no explicit WriteHeader call"))
		}
		handler := LogRequests(http.HandlerFunc(next))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/reminders", nil))

		if !strings.Contains(buf.String(), "status=200") {
			t.Errorf("log output = %q, want it to record status 200", buf.String())
		}
	})

	t.Run("first WriteHeader call wins, a second is a no-op", func(t *testing.T) {
		var buf bytes.Buffer
		restore := swapDefaultLogger(&buf)
		defer restore()

		next := func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.WriteHeader(http.StatusOK) // must not overwrite the recorded status
		}
		handler := LogRequests(http.HandlerFunc(next))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/reminders", nil))

		if !strings.Contains(buf.String(), "status=500") {
			t.Errorf("log output = %q, want the first WriteHeader's status (500) to stick", buf.String())
		}
	})

	t.Run("Write before WriteHeader locks in the implicit status", func(t *testing.T) {
		var buf bytes.Buffer
		restore := swapDefaultLogger(&buf)
		defer restore()

		next := func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("body first"))
			w.WriteHeader(http.StatusInternalServerError) // must be a no-op: Write already committed 200
		}
		handler := LogRequests(http.HandlerFunc(next))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/reminders", nil))

		if !strings.Contains(buf.String(), "status=200") {
			t.Errorf("log output = %q, want the implicit 200 from Write to stick despite a later WriteHeader call", buf.String())
		}
	})
}

func swapDefaultLogger(buf *bytes.Buffer) func() {
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(original) }
}
