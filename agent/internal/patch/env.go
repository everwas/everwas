package patch

import (
	"sort"
	"strings"
)

// envAllowlist is the set of host environment variables a package manager
// inherits. Same approach as internal/scripts: everything else (agent
// secrets, CI tokens, an operator's half-configured proxy) is dropped so a
// patch run cannot be steered by ambient state. Names are compared
// case-insensitively because Windows environment variables are.
//
// http_proxy and friends are deliberately IN the list: a host behind a proxy
// cannot reach its package mirrors without them, and they are set by the
// machine's own configuration, not by a job payload.
var envAllowlist = []string{
	// unix + common
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "TMPDIR",
	"http_proxy", "https_proxy", "ftp_proxy", "no_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY",
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
// applies extras, which always win. The result is sorted so a patch run's
// environment is reproducible and easy to assert in tests.
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

// noninteractiveEnv is what every package manager invocation gets on top of
// the allowlist: no prompts, no pager, C locale so parsers see stable
// English output regardless of the host's language.
func noninteractiveEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"DEBIAN_FRONTEND": "noninteractive",
		"LC_ALL":          "C",
		"LANG":            "C",
		"PAGER":           "cat",
		"TERM":            "dumb",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}
