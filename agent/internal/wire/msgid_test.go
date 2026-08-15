package wire

import (
	"strings"
	"testing"
	"time"
)

func TestNewMsgIDShape(t *testing.T) {
	id := NewMsgID()
	if len(id) != 26 {
		t.Fatalf("len = %d, want 26: %q", len(id), id)
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(crockford, rune(id[i])) {
			t.Errorf("char %q at %d not in Crockford alphabet", id[i], i)
		}
	}
}

func TestNewMsgIDUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewMsgID()
		if seen[id] {
			t.Fatalf("duplicate msg_id %q", id)
		}
		seen[id] = true
	}
}

func TestNewMsgIDTimeOrdered(t *testing.T) {
	a := NewMsgID()
	time.Sleep(3 * time.Millisecond)
	b := NewMsgID()
	if !(a < b) {
		t.Errorf("ids not time-ordered: %q then %q", a, b)
	}
}
