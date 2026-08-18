package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/netcert"
)

func TestTheWarningIsWrittenForAPersonNotAnOperator(t *testing.T) {
	// Whoever reads this is not an administrator. They are someone whose
	// laptop is about to stop working, and the message has to tell them what
	// will happen and the one thing they can do. A serial number, a file path
	// or the word "certificate authority" spends their attention on something
	// they cannot act on.
	now := time.Now()
	title, body := certificateWarning(netcert.PhaseUrgent, now.Add(72*time.Hour), now)

	for _, forbidden := range []string{
		"netcert", "802.1X", "EAP-TLS", "CSR", "serial", "/etc/", "C:\\", "PhaseUrgent",
	} {
		if strings.Contains(title+body, forbidden) {
			t.Errorf("the message contains %q, which means nothing to the person reading it", forbidden)
		}
	}
	// It must say what to do, not merely that something is wrong.
	if !strings.Contains(strings.ToLower(body), "connect") {
		t.Error("the message does not tell the user what action to take")
	}
	if !strings.Contains(body, "in 3 days") {
		t.Errorf("the message does not say when: %q", body)
	}
}

func TestAnExpiredCertificateSaysSoRatherThanCountingDown(t *testing.T) {
	now := time.Now()
	_, body := certificateWarning(netcert.PhaseExpired, now.Add(-time.Hour), now)
	if !strings.Contains(body, "has expired") {
		t.Errorf("an already-expired certificate did not say so: %q", body)
	}
	// A negative countdown would be worse than saying nothing.
	if strings.Contains(body, "-") {
		t.Errorf("the message contains a negative duration: %q", body)
	}
}

func TestDeadlinesReadTheWayPeopleSpeak(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Hour, "today"},
		{2 * time.Hour, "today"},
		{26 * time.Hour, "tomorrow"},
		{72 * time.Hour, "in 3 days"},
	} {
		if got := humanDeadline(tc.d); got != tc.want {
			t.Errorf("humanDeadline(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
