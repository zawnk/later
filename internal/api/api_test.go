package api

import (
	"reflect"
	"testing"
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
