package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zawnk/later/internal/reminder"
	"github.com/zawnk/later/internal/store"
)

// newServiceOverDataDir builds the service over the real store the way
// cmd/later-server does, so these tests exercise the actual stubs.json
// wiring rather than a hand-written double of it.
func newServiceOverDataDir(t *testing.T, stubsFile string) *Service {
	t.Helper()

	dir := t.TempDir()
	if stubsFile != "" {
		if err := os.WriteFile(filepath.Join(dir, "stubs.json"), []byte(stubsFile), 0600); err != nil {
			t.Fatalf("failed to write stubs file: %v", err)
		}
	}

	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}

	svc := New(s)
	svc.now = func() time.Time { return stubNow }
	return svc
}

func TestCreateReminder_ResolvesAStubFromTheDataDir(t *testing.T) {
	svc := newServiceOverDataDir(t, `{"hockey": {"text": "in 15m back to the game", "tags": ["hockey"], "priority": "high"}}`)

	rem, err := svc.CreateReminder(CreateInput{Text: ":hockey"})
	if err != nil {
		t.Fatalf("CreateReminder(\":hockey\") error = %v", err)
	}
	if rem.Text != "back to the game" {
		t.Errorf("CreateReminder() Text = %q, want the stub's text", rem.Text)
	}
	if want := stubNow.Add(15 * time.Minute); !rem.DueAt.Equal(want) {
		t.Errorf("CreateReminder() DueAt = %v, want %v", rem.DueAt, want)
	}
	if len(rem.Tags) != 1 || rem.Tags[0] != "hockey" || rem.Priority != "high" {
		t.Errorf("CreateReminder() tags/priority = %v/%q, want [hockey]/high", rem.Tags, rem.Priority)
	}
}

func TestCreateReminder_StubsFileAbsentOrMalformed(t *testing.T) {
	tests := []struct {
		name      string
		stubsFile string
		wantErr   string
	}{
		{"no stubs file at all", "", "unknown stub"},
		{"malformed stubs file", "{not valid json", "stubs.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newServiceOverDataDir(t, tt.stubsFile)

			if _, err := svc.CreateReminder(CreateInput{Text: "buy milk in 3 days"}); err != nil {
				t.Fatalf("CreateReminder() error = %v, want a plain reminder unaffected by the stubs file", err)
			}

			_, err := svc.CreateReminder(CreateInput{Text: ":hockey"})
			if err == nil {
				t.Fatalf("CreateReminder(\":hockey\") error = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CreateReminder() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestStubCRUD_OverTheDataDir walks define -> list -> invoke -> delete
// against the real store, so the write side and the read side are
// checked to agree on the file rather than only on a double of it.
func TestStubCRUD_OverTheDataDir(t *testing.T) {
	svc := newServiceOverDataDir(t, "")

	if _, err := svc.SetStub("hockey", reminder.Stub{Text: "in 15m back to the game", Tags: []string{"hockey"}, Priority: "high"}); err != nil {
		t.Fatalf("SetStub() error = %v", err)
	}
	if _, err := svc.SetStub("laundry", reminder.Stub{Text: "in 45m move the laundry"}); err != nil {
		t.Fatalf("SetStub() error = %v", err)
	}

	stubs, err := svc.ListStubs()
	if err != nil {
		t.Fatalf("ListStubs() error = %v", err)
	}
	if len(stubs) != 2 || stubs[0].Name != "hockey" || stubs[1].Name != "laundry" {
		t.Fatalf("ListStubs() = %+v, want hockey then laundry", stubs)
	}

	rem, err := svc.CreateReminder(CreateInput{Text: ":hockey"})
	if err != nil {
		t.Fatalf("CreateReminder(\":hockey\") error = %v, want the stub just written to be invocable", err)
	}
	if rem.Text != "back to the game" || rem.Priority != "high" {
		t.Errorf("CreateReminder() = %q/%q, want the written stub's text and priority", rem.Text, rem.Priority)
	}

	if err := svc.DeleteStub("hockey"); err != nil {
		t.Fatalf("DeleteStub() error = %v", err)
	}
	if _, err := svc.CreateReminder(CreateInput{Text: ":hockey"}); err == nil {
		t.Error("CreateReminder(\":hockey\") succeeded after the delete, want an unknown-stub error")
	}
	if _, err := svc.CreateReminder(CreateInput{Text: ":laundry"}); err != nil {
		t.Errorf("CreateReminder(\":laundry\") error = %v, want the untouched sibling to survive the delete", err)
	}
}
