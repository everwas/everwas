package svc

import (
	"encoding/xml"
	"strings"
)

// RenderSystemdUnit produces the openrmm-agent.service file. It is pure so it
// can be tested on any OS and diffed against packaging/linux/openrmm-agent.service.
//
// Two hardening choices are deliberate and load bearing:
//
// NoNewPrivileges is left off because the agent shells out to the platform
// package managers to apply patches, and those need to gain privileges.
// Setting it to true silently breaks patching, which is the failure mode
// nobody notices until an audit.
//
// Type is simple rather than notify: the agent does not link sd_notify, and
// claiming notify would make systemd wait for a readiness message that never
// arrives and then kill the unit.
func RenderSystemdUnit(cfg InstallConfig) string {
	cfg = cfg.normalized()
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + Description + "\n")
	b.WriteString("Documentation=https://github.com/rsp2k/openrmm/agent\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("# The rollback guard runs OUTSIDE the agent, which is the only way to\n")
	b.WriteString("# recover a build that cannot execute at all: wrong architecture, missing\n")
	b.WriteString("# symbol, panic in package init, a config the new binary cannot parse.\n")
	b.WriteString("# None of those ever reach the agent's own crash counter. The leading '-'\n")
	b.WriteString("# and the /bin/sh wrapper mean a missing guard can never block startup.\n")
	b.WriteString("ExecStartPre=-" + systemdGuardExec(cfg) + "\n")
	b.WriteString("ExecStart=" + systemdExec(cfg) + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	if cfg.StateDir != "" {
		b.WriteString("Environment=" + StateDirEnv + "=" + cfg.StateDir + "\n")
	}
	b.WriteString("\n")
	b.WriteString("# The agent applies OS patches, so it runs as root and must keep the\n")
	b.WriteString("# ability to gain privileges. Forbidding that here breaks apt, dnf and\n")
	b.WriteString("# zypper in ways that are hard to spot from the outside.\n")
	b.WriteString("NoNewPrivileges=false\n")
	b.WriteString("ProtectHome=read-only\n")
	b.WriteString("PrivateTmp=true\n")
	b.WriteString("\n")
	b.WriteString("# Package managers run in a transient scope (systemd-run --scope), so they\n")
	b.WriteString("# are NOT members of this cgroup: a restart of the agent during a patch\n")
	b.WriteString("# window would otherwise SIGKILL dpkg mid-transaction and leave the package\n")
	b.WriteString("# database for a human to repair. TimeoutStopSec is generous for the\n")
	b.WriteString("# fallback case, where systemd-run is unavailable and the transaction\n")
	b.WriteString("# really is in this cgroup.\n")
	b.WriteString("KillMode=mixed\n")
	b.WriteString("TimeoutStopSec=300\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// systemdExec joins the binary and its arguments. systemd splits ExecStart on
// whitespace unless the word is quoted, so anything containing a space gets
// double quoted with backslashes escaped.
func systemdExec(cfg InstallConfig) string {
	parts := make([]string, 0, len(cfg.Args)+1)
	for _, w := range append([]string{cfg.BinaryPath}, cfg.Args...) {
		parts = append(parts, systemdQuote(w))
	}
	return strings.Join(parts, " ")
}

// systemdGuardExec renders the ExecStartPre line. It goes through /bin/sh
// rather than executing the guard directly so that a host where the guard was
// never installed fails on a shell that always exists, which combined with the
// '-' prefix keeps the agent starting.
func systemdGuardExec(cfg InstallConfig) string {
	parts := []string{"/bin/sh", LinuxGuardPath, "check", cfg.BinaryPath}
	for i, p := range parts {
		parts[i] = systemdQuote(p)
	}
	return strings.Join(parts, " ")
}

func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// RenderLaunchdPlist produces the com.openrmm.agent.plist file. KeepAlive
// plus RunAtLoad is launchd's equivalent of Restart=always, and
// ThrottleInterval stops a crash looping agent from spinning the CPU.
func RenderLaunchdPlist(cfg InstallConfig) string {
	cfg = cfg.normalized()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + xmlEscape(LaunchdLabel) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range launchdProgramArguments(cfg) {
		b.WriteString("    <string>" + xmlEscape(arg) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>ThrottleInterval</key>\n  <integer>5</integer>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(MacLogDir+"/agent.log") + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(MacLogDir+"/agent.err.log") + "</string>\n")
	if cfg.StateDir != "" {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		b.WriteString("    <key>" + StateDirEnv + "</key>\n    <string>" + xmlEscape(cfg.StateDir) + "</string>\n")
		b.WriteString("  </dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// launchdProgramArguments wraps the agent in the rollback guard. launchd has
// no ExecStartPre, so the guard has to be what launchd starts, and it execs
// the agent when it is done: same pid, so KeepAlive still watches the agent
// rather than a wrapper.
//
// The guard is invoked through a shell test rather than directly. A daemon
// whose ProgramArguments[0] does not exist never starts at all, and "the
// rollback guard is missing" must not be a way to lose the agent.
func launchdProgramArguments(cfg InstallConfig) []string {
	agent := append([]string{cfg.BinaryPath}, cfg.Args...)
	quoted := make([]string, 0, len(agent))
	for _, a := range agent {
		quoted = append(quoted, shQuote(a))
	}
	guard := shQuote(DarwinGuardPath)
	line := "[ -x " + guard + " ] && " + guard + " check " + shQuote(cfg.BinaryPath) +
		"; exec " + strings.Join(quoted, " ")
	return []string{"/bin/sh", "-c", line}
}

// shQuote makes a value safe inside a single shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails when the writer fails, and strings.Builder
		// never does. Fall back to the raw value rather than losing it.
		return s
	}
	return b.String()
}
