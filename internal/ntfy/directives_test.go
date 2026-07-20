package ntfy

import (
	"errors"
	"slices"
	"testing"
)

func TestParseDirectives(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantText     string
		wantTags     []string
		wantPriority string
		wantErr      error
	}{
		{
			name:     "no directives",
			in:       "buy milk in 3 days",
			wantText: "buy milk in 3 days",
		},
		{
			name:     "single trailing tag",
			in:       "buy milk in 3 days #groceries",
			wantText: "buy milk in 3 days",
			wantTags: []string{"groceries"},
		},
		{
			name:     "multiple trailing tags preserve left-to-right order",
			in:       "buy milk in 3 days #groceries #urgent",
			wantText: "buy milk in 3 days",
			wantTags: []string{"groceries", "urgent"},
		},
		{
			name:     "multiple same tags get deduplicated",
			in:       "buy milk in 3 days #groceries #groceries",
			wantText: "buy milk in 3 days",
			wantTags: []string{"groceries"},
		},
		{
			name:         "trailing priority word",
			in:           "call mom tomorrow at 5 !high",
			wantText:     "call mom tomorrow at 5",
			wantPriority: "high",
		},
		{
			name:         "trailing priority digit form stored as-is (no normalization)",
			in:           "call mom tomorrow at 5 !4",
			wantText:     "call mom tomorrow at 5",
			wantPriority: "4",
		},
		{
			name:         "tag then priority, mixed order",
			in:           "buy milk tomorrow #groceries !low",
			wantText:     "buy milk tomorrow",
			wantTags:     []string{"groceries"},
			wantPriority: "low",
		},
		{
			name:         "priority then tag, order doesn't matter",
			in:           "buy milk tomorrow !low #groceries",
			wantText:     "buy milk tomorrow",
			wantTags:     []string{"groceries"},
			wantPriority: "low",
		},
		{
			name:     "tag, priority, tag interleaved - tags stay in original order",
			in:       "meeting tomorrow #work !high #urgent",
			wantText: "meeting tomorrow",
			wantTags: []string{"work", "urgent"},

			wantPriority: "high",
		},
		{
			name:         "invalid trailing token stops the scan (left as literal)",
			in:           "buy milk tomorrow !bogus #groceries",
			wantText:     "buy milk tomorrow !bogus",
			wantTags:     []string{"groceries"},
			wantPriority: "",
		},
		{
			name:     "non-directive last token stops immediately, nothing stripped",
			in:       "renew license !urgentt tomorrow",
			wantText: "renew license !urgentt tomorrow",
		},
		{
			name:     "out-of-range digit priority is not a valid directive",
			in:       "buy milk tomorrow !6",
			wantText: "buy milk tomorrow !6",
		},
		{
			name:     "tag case is preserved, not lowercased",
			in:       "buy milk tomorrow #Family",
			wantText: "buy milk tomorrow",
			wantTags: []string{"Family"},
		},
		{
			name:     "internal double spaces collapse to single spaces",
			in:       "buy   milk  tomorrow #groceries",
			wantText: "buy milk tomorrow",
			wantTags: []string{"groceries"},
		},
		{
			name:    "two different priority directives conflict",
			in:      "finish report tomorrow !high !low",
			wantErr: ErrConflictingPriority,
		},
		{
			name:    "two identical priority directives still conflict (no same-value carve-out)",
			in:      "finish report tomorrow !high !high",
			wantErr: ErrConflictingPriority,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotTags, gotPriority, err := parseDirectives(tt.in)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parseDirectives(%q) error = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDirectives(%q) error = %v, want nil", tt.in, err)
			}

			if gotText != tt.wantText {
				t.Errorf("parseDirectives(%q) text = %q, want %q", tt.in, gotText, tt.wantText)
			}
			if !slices.Equal(gotTags, tt.wantTags) {
				t.Errorf("parseDirectives(%q) tags = %v, want %v", tt.in, gotTags, tt.wantTags)
			}
			if gotPriority != tt.wantPriority {
				t.Errorf("parseDirectives(%q) priority = %q, want %q", tt.in, gotPriority, tt.wantPriority)
			}
		})
	}
}
