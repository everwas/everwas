package wire

import (
	"errors"
	"strings"
	"testing"
)

func TestValidIdentifier(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{"uuid", "01991111-2222-7333-8444-555566667777", true},
		{"slug", "session_1", true},
		{"dotted version-like id", "agent.v1.2", true},
		{"single char", "a", true},
		{"max length", strings.Repeat("a", MaxIdentifierLen), true},

		{"empty", "", false},
		{"full wildcard", ">", false},
		{"token wildcard", "*", false},
		{"wildcard buried in a real id", "sid-1>", false},
		{"wildcard token", "a.*.b", false},
		{"dot", ".", false},
		{"dot dot", "..", false},
		{"leading dot", ".hidden", false},
		{"trailing dot", "sid.", false},
		{"empty middle segment", "a..b", false},
		{"space", "a b", false},
		{"tab", "a\tb", false},
		{"newline", "a\nb", false},
		{"slash", "a/b", false},
		{"colon", "sched:e1:12345", false},
		{"nul", "a\x00b", false},
		{"non ascii", "sessión", false},
		{"too long", strings.Repeat("a", MaxIdentifierLen+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidIdentifier(tt.id)
			if tt.ok && err != nil {
				t.Fatalf("ValidIdentifier(%q) = %v, want nil", tt.id, err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("ValidIdentifier(%q) = nil, want a rejection", tt.id)
				}
				if !errors.Is(err, ErrInvalidIdentifier) {
					t.Fatalf("ValidIdentifier(%q) = %v, want it to wrap ErrInvalidIdentifier", tt.id, err)
				}
			}
		})
	}
}

// TestValidIdentifierRejectsSubjectBreakers is the one that matters: each of
// these ids, interpolated into a subject, either widens the subscription or
// makes the server close the connection permanently.
func TestValidIdentifierRejectsSubjectBreakers(t *testing.T) {
	for _, id := range []string{">", "*", "a.>", "a.*", "in.>"} {
		if err := ValidIdentifier(id); err == nil {
			t.Errorf("ValidIdentifier(%q) accepted a subject-breaking id; %q would be built",
				id, ShellIn("agent", id))
		}
	}
}

func TestCheckIdentifierNamesTheField(t *testing.T) {
	err := CheckIdentifier("session_id", ">")
	if err == nil {
		t.Fatal("CheckIdentifier accepted \">\"")
	}
	if !strings.Contains(err.Error(), "session_id") {
		t.Errorf("err = %q, want it to name session_id", err)
	}
	if !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("err = %v, want it to wrap ErrInvalidIdentifier", err)
	}
	if err := CheckIdentifier("session_id", "sid-1"); err != nil {
		t.Errorf("CheckIdentifier rejected a good id: %v", err)
	}
}
