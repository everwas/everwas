package scripts

import (
	"slices"
	"strings"
	"testing"
)

func TestScrubEnvAllowlist(t *testing.T) {
	host := []string{
		"PATH=/usr/bin",
		"HOME=/home/agent",
		"USER=agent",
		"LANG=en_US.UTF-8",
		"AWS_SECRET_ACCESS_KEY=hunter2",
		"EVERWAS_AGENT_SECRET=s3cr3t",
		"SSH_AUTH_SOCK=/run/agent.sock",
		"NOT_AN_ASSIGNMENT",
		"=novalue",
	}
	got := scrubEnv(host, nil)
	want := []string{"HOME=/home/agent", "LANG=en_US.UTF-8", "PATH=/usr/bin", "USER=agent"}
	if !slices.Equal(got, want) {
		t.Fatalf("scrubEnv = %v, want %v", got, want)
	}
}

func TestScrubEnvWindowsNamesAreCaseInsensitive(t *testing.T) {
	host := []string{"SystemRoot=C:\\Windows", "systemdrive=C:", "ComSpec=C:\\Windows\\cmd.exe",
		"PATHEXT=.COM;.EXE", "TEMP=C:\\Temp", "TMP=C:\\Temp", "SECRET_TOKEN=nope"}
	got := scrubEnv(host, nil)
	for _, kv := range got {
		if strings.HasPrefix(kv, "SECRET_TOKEN") {
			t.Fatalf("secret survived scrubbing: %v", got)
		}
	}
	if len(got) != 6 {
		t.Errorf("kept %d vars, want 6: %v", len(got), got)
	}
}

func TestScrubEnvExtras(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]string
		want  []string
	}{
		{"extras are added", map[string]string{"JOB_ID": "j1"},
			[]string{"JOB_ID=j1", "PATH=/usr/bin"}},
		{"extras override the host", map[string]string{"PATH": "/opt/bin"},
			[]string{"PATH=/opt/bin"}},
		{"extras may be non-allowlisted", map[string]string{"API_TOKEN": "t"},
			[]string{"API_TOKEN=t", "PATH=/usr/bin"}},
		{"empty name is dropped", map[string]string{"": "x"},
			[]string{"PATH=/usr/bin"}},
		{"name with = is dropped", map[string]string{"A=B": "x"},
			[]string{"PATH=/usr/bin"}},
		{"name with NUL is dropped", map[string]string{"A\x00B": "x"},
			[]string{"PATH=/usr/bin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrubEnv([]string{"PATH=/usr/bin"}, tt.extra)
			if !slices.Equal(got, tt.want) {
				t.Errorf("scrubEnv = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScrubEnvIsSorted(t *testing.T) {
	got := scrubEnv([]string{"USER=a", "PATH=b", "HOME=c"}, nil)
	if !slices.IsSorted(got) {
		t.Errorf("scrubEnv output not sorted: %v", got)
	}
}
