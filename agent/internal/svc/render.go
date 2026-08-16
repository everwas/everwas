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
	b.WriteString("KillMode=mixed\n")
	b.WriteString("TimeoutStopSec=30\n")
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
	for _, arg := range append([]string{cfg.BinaryPath}, cfg.Args...) {
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

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails when the writer fails, and strings.Builder
		// never does. Fall back to the raw value rather than losing it.
		return s
	}
	return b.String()
}
