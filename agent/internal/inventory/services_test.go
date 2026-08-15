package inventory

import "testing"

func TestParseSystemctlUnits(t *testing.T) {
	out := "cron.service loaded active running Regular background program processing daemon\n" +
		"dbus.service loaded active running D-Bus System Message Bus\n" +
		"getty@tty1.service loaded active running Getty on tty1\n" +
		"nginx.service loaded inactive dead A high performance web server\n" +
		"snapd.service loaded failed failed Snap Daemon\n" +
		"systemd-fsck@dev.service not-found inactive dead systemd-fsck@dev.service\n" +
		"short.line\n" + // not a .service unit
		"\n"
	got := parseSystemctlUnits(out)
	want := []svc{
		{"cron.service", "active"},
		{"dbus.service", "active"},
		{"getty@tty1.service", "active"},
		{"nginx.service", "inactive"},
		{"snapd.service", "failed"},
		{"systemd-fsck@dev.service", "inactive"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d services, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("service %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSystemctlUnitsEmpty(t *testing.T) {
	if got := parseSystemctlUnits(""); got == nil || len(got) != 0 {
		t.Errorf("empty input: got %#v, want empty non-nil slice", got)
	}
}
