package sched

import (
	"fmt"
	"testing"
	"time"
)

// TestJitterIsStable is the property that matters operationally: the same
// agent and entry must land on the same offset every night, so a slow
// Tuesday is reproducible instead of a fresh random mystery.
func TestJitterIsStable(t *testing.T) {
	const agent = "0198f6f2-0000-7000-8000-000000000001"
	first := Jitter(agent, "nightly-patch-scan", 3600)
	for i := 0; i < 1000; i++ {
		if got := Jitter(agent, "nightly-patch-scan", 3600); got != first {
			t.Fatalf("call %d returned %s, want %s", i, got, first)
		}
	}
}

func TestJitterVariesByAgentAndEntry(t *testing.T) {
	const jitterS = 3600
	base := Jitter("agent-a", "entry-1", jitterS)
	if Jitter("agent-b", "entry-1", jitterS) == base {
		t.Error("two agents landed on the same offset for the same entry")
	}
	if Jitter("agent-a", "entry-2", jitterS) == base {
		t.Error("two entries landed on the same offset for the same agent")
	}
}

func TestJitterBounds(t *testing.T) {
	tests := []struct {
		jitterS int
		want    time.Duration // exact expected value when deterministic
		exact   bool
	}{
		{jitterS: 0, want: 0, exact: true},
		{jitterS: -1, want: 0, exact: true},
		{jitterS: 1, want: 0, exact: true}, // n % 1 == 0
		{jitterS: 3600},
		{jitterS: 60},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("jitter_s=%d", tt.jitterS), func(t *testing.T) {
			got := Jitter("agent", "entry", tt.jitterS)
			if tt.exact && got != tt.want {
				t.Fatalf("Jitter = %s, want %s", got, tt.want)
			}
			if got < 0 || (tt.jitterS > 0 && got >= time.Duration(tt.jitterS)*time.Second) {
				t.Errorf("Jitter = %s, outside [0, %ds)", got, tt.jitterS)
			}
		})
	}
}

// TestJitterSpreadsFleet checks the offsets actually spread rather than
// clumping: 200 agents over a 1 hour window should hit many distinct minutes.
func TestJitterSpreadsFleet(t *testing.T) {
	buckets := map[int]int{}
	for i := 0; i < 200; i++ {
		d := Jitter(fmt.Sprintf("agent-%03d", i), "nightly", 3600)
		buckets[int(d.Minutes())]++
	}
	if len(buckets) < 40 {
		t.Errorf("200 agents landed in only %d distinct minutes", len(buckets))
	}
}

// TestJitterConcatenationIsUnambiguousEnough documents a known property:
// hashing agent+entry as a plain concatenation means ("ab","c") and
// ("a","bc") collide. Entry ids are server-generated and never split that
// way, so this is recorded rather than defended against.
func TestJitterConcatenationCollision(t *testing.T) {
	if Jitter("ab", "c", 3600) != Jitter("a", "bc", 3600) {
		t.Skip("implementation no longer hashes a plain concatenation")
	}
}
