package scripts

import (
	"sort"
	"strings"
)

// envAllowlist is the set of host environment variables a job inherits.
// Everything else (credentials, agent secrets, CI tokens, proxy configs the
// operator did not ask for) is dropped. Names are compared case-insensitively
// because Windows environment variables are case-insensitive.
var envAllowlist = []string{
	// unix + common
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "SHELL", "TMPDIR",
	// windows
	"TEMP", "TMP", "SystemRoot", "SystemDrive", "ComSpec", "PATHEXT",
	"windir", "USERPROFILE", "USERNAME", "PROGRAMFILES", "PROGRAMDATA",
	"APPDATA", "LOCALAPPDATA", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
}

var envAllowSet = func() map[string]bool {
	m := make(map[string]bool, len(envAllowlist))
	for _, k := range envAllowlist {
		m[strings.ToLower(k)] = true
	}
	return m
}()

// scrubEnv filters hostEnv ("K=V" pairs) down to the allowlist and then
// applies the server-provided extras, which always win. The result is sorted
// so a job's environment is reproducible and easy to assert in tests.
func scrubEnv(hostEnv []string, extra map[string]string) []string {
	kept := make(map[string]string, len(envAllowlist)+len(extra))
	for _, kv := range hostEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !envAllowSet[strings.ToLower(k)] {
			continue
		}
		kept[k] = v
	}
	for k, v := range extra {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			continue // a name with '=' would forge a second variable
		}
		kept[k] = v
	}
	out := make([]string, 0, len(kept))
	for k, v := range kept {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
