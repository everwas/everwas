package svc

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise rendering only. Nothing here installs a service,
// touches systemd or launchd, or writes outside a temp directory.

func TestRenderSystemdUnit(t *testing.T) {
	unit := RenderSystemdUnit(InstallConfig{
		BinaryPath: "/usr/local/bin/openrmm-agent",
		StateDir:   "/etc/openrmm",
	})

	want := []string{
		"[Unit]",
		"Description=" + Description,
		"After=network-online.target",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/openrmm-agent run",
		"Restart=always",
		"RestartSec=5",
		"NoNewPrivileges=false",
		"ProtectHome=read-only",
		"PrivateTmp=true",
		"Environment=OPENRMM_STATE_DIR=/etc/openrmm",
		"[Install]",
		"WantedBy=multi-user.target",
	}
	for _, w := range want {
		if !strings.Contains(unit, w) {
			t.Errorf("unit is missing %q\n---\n%s", w, unit)
		}
	}
	// Type=notify would make systemd wait for an sd_notify the agent never
	// sends, so it must never appear.
	if strings.Contains(unit, "Type=notify") {
		t.Error("unit must not use Type=notify")
	}
	if strings.Contains(unit, "NoNewPrivileges=true") {
		t.Error("NoNewPrivileges=true breaks patching")
	}
}

func TestRenderSystemdUnitDefaultsAndQuoting(t *testing.T) {
	unit := RenderSystemdUnit(InstallConfig{BinaryPath: "/opt/open rmm/openrmm-agent"})
	if !strings.Contains(unit, `ExecStart="/opt/open rmm/openrmm-agent" run`) {
		t.Errorf("a path with a space must be quoted\n---\n%s", unit)
	}
	if strings.Contains(unit, "Environment=OPENRMM_STATE_DIR") {
		t.Error("no state dir override means no Environment line")
	}

	custom := RenderSystemdUnit(InstallConfig{BinaryPath: "/usr/local/bin/openrmm-agent", Args: []string{"run", "--verbose"}})
	if !strings.Contains(custom, "ExecStart=/usr/local/bin/openrmm-agent run --verbose") {
		t.Errorf("custom args not rendered\n---\n%s", custom)
	}
}

func TestRenderedUnitMatchesPackagedUnit(t *testing.T) {
	// The packaged unit and the one the installer writes have to agree, or a
	// host installed from the deb behaves differently from one installed with
	// `openrmm-agent install`.
	rendered := RenderSystemdUnit(InstallConfig{BinaryPath: "/usr/local/bin/openrmm-agent"})
	packaged, err := readIfExists(filepath.Join("..", "..", "packaging", "linux", "openrmm-agent.service"))
	if err != nil {
		t.Fatalf("read packaged unit: %v", err)
	}
	if packaged == "" {
		t.Skip("packaged unit not present")
	}
	if normalizeUnit(rendered) != normalizeUnit(packaged) {
		t.Errorf("packaged unit differs from the rendered one\n--- rendered ---\n%s\n--- packaged ---\n%s", rendered, packaged)
	}
}

func TestRenderLaunchdPlistIsValidXML(t *testing.T) {
	plist := RenderLaunchdPlist(InstallConfig{
		BinaryPath: "/Library/OpenRMM/Agent/openrmm-agent",
		StateDir:   "/Library/Application Support/OpenRMM",
	})

	var doc struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
		t.Fatalf("plist is not valid XML: %v\n---\n%s", err, plist)
	}

	want := []string{
		"<key>Label</key>",
		"<string>com.openrmm.agent</string>",
		"<string>/Library/OpenRMM/Agent/openrmm-agent</string>",
		"<string>run</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>ThrottleInterval</key>",
		"<string>/Library/Logs/OpenRMM/agent.log</string>",
		"<string>/Library/Logs/OpenRMM/agent.err.log</string>",
		"<key>OPENRMM_STATE_DIR</key>",
	}
	for _, w := range want {
		if !strings.Contains(plist, w) {
			t.Errorf("plist is missing %q\n---\n%s", w, plist)
		}
	}
}

func TestRenderLaunchdPlistEscapesXML(t *testing.T) {
	plist := RenderLaunchdPlist(InstallConfig{BinaryPath: `/opt/a&b/<agent>`})
	if strings.Contains(plist, "<agent>") {
		t.Error("angle brackets in a path must be escaped")
	}
	if !strings.Contains(plist, "&amp;") {
		t.Error("ampersand in a path must be escaped")
	}
	var doc struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
		t.Fatalf("escaped plist is not valid XML: %v", err)
	}
}

func TestRenderedPlistMatchesPackagedPlist(t *testing.T) {
	rendered := RenderLaunchdPlist(InstallConfig{BinaryPath: "/Library/OpenRMM/Agent/openrmm-agent"})
	packaged, err := readIfExists(filepath.Join("..", "..", "packaging", "darwin", "com.openrmm.agent.plist"))
	if err != nil {
		t.Fatalf("read packaged plist: %v", err)
	}
	if packaged == "" {
		t.Skip("packaged plist not present")
	}
	if normalizeUnit(rendered) != normalizeUnit(packaged) {
		t.Errorf("packaged plist differs from the rendered one\n--- rendered ---\n%s\n--- packaged ---\n%s", rendered, packaged)
	}
}

func TestInstallConfigDefaults(t *testing.T) {
	t.Setenv(StateDirEnv, "/tmp/openrmm-test-state")
	t.Setenv(PrefixEnv, "/tmp/openrmm-test-prefix")

	got := InstallConfig{BinaryPath: "/usr/local/bin/openrmm-agent"}.normalized()
	if len(got.Args) != 1 || got.Args[0] != "run" {
		t.Errorf("Args = %v, want [run]", got.Args)
	}
	if got.StateDir != "/tmp/openrmm-test-state" {
		t.Errorf("StateDir = %s, want the env override", got.StateDir)
	}
	if got.Prefix != "/tmp/openrmm-test-prefix" {
		t.Errorf("Prefix = %s, want the env override", got.Prefix)
	}

	explicit := InstallConfig{BinaryPath: "/x", StateDir: "/explicit", Prefix: "/p"}.normalized()
	if explicit.StateDir != "/explicit" || explicit.Prefix != "/p" {
		t.Errorf("explicit values must win over the environment, got %+v", explicit)
	}
}

func TestValidateRequiresBinary(t *testing.T) {
	if err := (InstallConfig{}).validate(); err == nil {
		t.Error("an empty binary path must be rejected")
	}
	if err := (InstallConfig{BinaryPath: "   "}).validate(); err == nil {
		t.Error("a blank binary path must be rejected")
	}
}

func TestServicePathsHonourPrefix(t *testing.T) {
	root := t.TempDir()
	if got := UnitPath(root); !strings.HasPrefix(got, root) {
		t.Errorf("UnitPath = %s, want it under %s", got, root)
	}
	if got := PlistPath(root); !strings.HasPrefix(got, root) {
		t.Errorf("PlistPath = %s, want it under %s", got, root)
	}
	if got := UnitPath(""); got != filepath.FromSlash("/etc/systemd/system/openrmm-agent.service") {
		t.Errorf("UnitPath = %s, want the real system location", got)
	}
	if got := PlistPath(""); got != filepath.FromSlash("/Library/LaunchDaemons/com.openrmm.agent.plist") {
		t.Errorf("PlistPath = %s, want the real system location", got)
	}
}

// normalizeUnit strips comment lines and trailing whitespace so the
// packaged and rendered files can be compared on content alone.
func normalizeUnit(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func readIfExists(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}
