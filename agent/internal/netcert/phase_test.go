package netcert

import (
	"fmt"
	"testing"
	"time"
)

func lifetime(days int) *Material {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Material{NotBefore: start, NotAfter: start.Add(time.Duration(days) * 24 * time.Hour)}
}

func at(m *Material, fraction float64) time.Time {
	life := m.NotAfter.Sub(m.NotBefore)
	return m.NotBefore.Add(time.Duration(float64(life) * fraction))
}

func TestThePhasesFollowTheCertificatesLife(t *testing.T) {
	m := lifetime(90)
	const id = "device-a"

	for _, tc := range []struct {
		name     string
		fraction float64
		want     Phase
	}{
		{"brand new", 0.01, PhaseFresh},
		{"just before half life", 0.49, PhaseFresh},
		// 0.56 clears half life plus the largest possible jitter offset.
		{"past half life and jitter", 0.56, PhaseDue},
		{"most of the way through", 0.80, PhaseDue},
		{"past the urgent point", 0.90, PhaseUrgent},
		{"just before expiry", 0.999, PhaseUrgent},
		{"expired", 1.01, PhaseExpired},
	} {
		if got := m.PhaseAt(at(m, tc.fraction), id); got != tc.want {
			t.Errorf("%s (%.2f of life): phase = %v, want %v", tc.name, tc.fraction, got, tc.want)
		}
	}
}

func TestHoldingNothingIsTheMostUrgentAnswer(t *testing.T) {
	// A device with no certificate at all is the case escalation exists for,
	// so it must not read as "fresh, nothing to do".
	var m *Material
	if got := m.PhaseAt(time.Now(), "device-a"); got != PhaseExpired {
		t.Errorf("phase = %v, want PhaseExpired", got)
	}
}

func TestJitterNeverDelaysRenewalIntoTheUrgentWindow(t *testing.T) {
	// The invariant that makes the jitter safe. Renewal must always begin
	// while there is still routine margin left; if RenewJitter were ever
	// raised past the gap to UrgentAt, some devices would go straight from
	// fresh to urgent and lose the entire relaxed retry phase without anyone
	// changing a line of renewal logic.
	if RenewAt+RenewJitter >= UrgentAt {
		t.Fatalf("RenewAt(%v) + RenewJitter(%v) >= UrgentAt(%v): some devices would "+
			"never get a routine renewal window", RenewAt, RenewJitter, UrgentAt)
	}
}

func TestTheJitterOffsetIsStableForADevice(t *testing.T) {
	// A device that re-rolled its offset could drift earlier on one check and
	// later on the next. The point is to separate devices from each other, not
	// a device from itself.
	for _, id := range []string{"device-a", "device-b", ""} {
		first := renewalOffset(id)
		for range 5 {
			if got := renewalOffset(id); got != first {
				t.Errorf("id %q: offset moved from %v to %v", id, first, got)
			}
		}
	}
}

func TestTheJitterSpreadsAFleetRatherThanStackingIt(t *testing.T) {
	// The failure this prevents: a hundred machines imaged the same afternoon
	// all renewing in the same window and stampeding the CA.
	buckets := map[int]int{}
	const fleet = 200
	for i := range fleet {
		off := renewalOffset(fmt.Sprintf("01a00b45-0e50-78c8-b572-8b8fbc2720%02d", i))
		if off < 0 || off >= RenewJitter {
			t.Fatalf("offset %v outside [0, %v)", off, RenewJitter)
		}
		// Tenths of the jitter span.
		buckets[int(off/RenewJitter*10)]++
	}
	if len(buckets) < 8 {
		t.Errorf("a fleet of %d landed in only %d of 10 buckets, which is not a spread",
			fleet, len(buckets))
	}
	for b, n := range buckets {
		if n > fleet/3 {
			t.Errorf("bucket %d holds %d of %d devices: the fleet is stacking", b, n, fleet)
		}
	}
}

func TestAWrongClockAsksForAReplacementWithoutRaisingAnAlarm(t *testing.T) {
	// A not-yet-valid certificate almost always means the clock is wrong.
	// Worth asking for a fresh one, which is harmless and surfaces it, but it
	// is not a deadline and must not read as urgent.
	m := lifetime(90)
	if got := m.PhaseAt(m.NotBefore.Add(-48*time.Hour), "device-a"); got != PhaseDue {
		t.Errorf("phase = %v, want PhaseDue", got)
	}
}

func TestAZeroLengthWindowIsExpiredNotFresh(t *testing.T) {
	now := time.Now()
	m := &Material{NotBefore: now, NotAfter: now}
	if got := m.PhaseAt(now, "device-a"); got != PhaseExpired {
		t.Errorf("phase = %v, want PhaseExpired", got)
	}
}
