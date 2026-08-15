package shell

import (
	"bytes"
	"testing"
)

func TestRingDropOldest(t *testing.T) {
	tests := []struct {
		name        string
		max         int
		writes      []string
		want        string
		wantDropped bool
	}{
		{"under capacity", 8, []string{"abc", "de"}, "abcde", false},
		{"exactly at capacity", 5, []string{"abc", "de"}, "abcde", false},
		{"drops the oldest byte", 5, []string{"abc", "def"}, "bcdef", true},
		{"single oversized write keeps the tail", 4, []string{"abcdefgh"}, "efgh", true},
		{"empty write is a no-op", 4, []string{""}, "", false},
		{"repeated overflow", 3, []string{"ab", "cd", "ef"}, "def", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRing(tt.max)
			for _, w := range tt.writes {
				r.write([]byte(w))
			}
			got, dropped := r.drain()
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Errorf("drain = %q, want %q", got, tt.want)
			}
			if dropped != tt.wantDropped {
				t.Errorf("dropped = %v, want %v", dropped, tt.wantDropped)
			}
		})
	}
}

func TestRingDrainResets(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcdef"))
	if _, dropped := r.drain(); !dropped {
		t.Fatal("want dropped on first drain")
	}
	if r.size() != 0 {
		t.Errorf("size after drain = %d, want 0", r.size())
	}
	got, dropped := r.drain()
	if len(got) != 0 || dropped {
		t.Errorf("second drain = %q/%v, want empty and not dropped", got, dropped)
	}
}
