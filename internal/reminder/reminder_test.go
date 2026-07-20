package reminder

import (
	"slices"
	"testing"
)

func TestIsValidPriority(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"min is valid", "min", true},
		{"low is valid", "low", true},
		{"default is valid", "default", true},
		{"high is valid", "high", true},
		{"urgent is valid", "urgent", true},
		{"max is valid", "max", true},
		{"digit 1 is valid", "1", true},
		{"digit 5 is valid", "5", true},
		{"empty string is invalid", "", false},
		{"unknown word is invalid", "bogus", false},
		{"digit 0 is invalid", "0", false},
		{"digit 6 is invalid", "6", false},
		{"case-sensitive: capitalized word is invalid", "High", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidPriority(tt.in); got != tt.want {
				t.Errorf("IsValidPriority(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDedupeStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"no tags", nil, nil},
		{"no duplicates", []string{"work", "urgent"}, []string{"work", "urgent"}},
		{"adjacent duplicate", []string{"work", "work"}, []string{"work"}},
		{"non-adjacent duplicate, first occurrence wins position", []string{"work", "urgent", "work"}, []string{"work", "urgent"}},
		{"all identical", []string{"work", "work", "work"}, []string{"work"}},
		{"case-sensitive: different case is not a duplicate", []string{"work", "Work"}, []string{"work", "Work"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DedupeStrings(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("DedupeStrings(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
