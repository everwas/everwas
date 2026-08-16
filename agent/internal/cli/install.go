package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/config"
	"github.com/rsp2k/openrmm/agent/internal/enroll"
	"github.com/rsp2k/openrmm/agent/internal/svc"
	"github.com/rsp2k/openrmm/agent/internal/update"
)

// CmdInstall copies the running binary to the per-OS install location,
// optionally enrolls it, then registers and starts the system service.
//
// It is safe to re-run: an existing install is overwritten and the service
// definition is rewritten in place.
func CmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	server := fs.String("server", "", "server base URL, e.g. https://rmm.example.com")
	token := fs.String("token", "", "one-time enrollment token")
	dest := fs.String("path", svc.DefaultInstallPath(), "where to install the agent binary")
	skipService := fs.Bool("skip-service", false, "install the binary and enroll, but do not touch the service manager")
	force := fs.Bool("force", false, "re-enroll even if this host already has an identity")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := requireAdmin(); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 1
	}
	if *dest == "" {
		fmt.Fprintf(os.Stderr, "install: no default install path for %s, pass --path\n", runtime.GOOS)
		return 2
	}
	if (*server == "") != (*token == "") {
		fmt.Fprintln(os.Stderr, "install: --server and --token must be given together")
		return 2
	}

	installed, err := installBinary(*dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 1
	}
	fmt.Printf("installed binary: %s\n", installed)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: read existing config: %v\n", err)
		return 1
	}
	switch {
	case *server != "" && (!cfg.Enrolled() || *force):
		if err := enroll.Enroll(*server, *token, Version); err != nil {
			fmt.Fprintf(os.Stderr, "install: enroll: %v\n", err)
			return 1
		}
		path, _ := config.Path()
		fmt.Printf("enrolled; identity saved to %s\n", path)
	case *server != "":
		fmt.Println("already enrolled; skipping enrollment (pass --force to re-enroll)")
	case !cfg.Enrolled():
		fmt.Println("not enrolled yet; the service will idle until you run:")
		fmt.Printf("  %s enroll --server URL --token TOKEN\n", installed)
	}

	if *skipService {
		fmt.Println("service registration skipped (--skip-service)")
		return 0
	}

	if err := svc.Install(svc.InstallConfig{BinaryPath: installed}); err != nil {
		fmt.Fprintf(os.Stderr, "install: register service: %v\n", err)
		return 1
	}
	fmt.Printf("service %q installed and started\n", svc.Name)
	printNextSteps(installed)
	return 0
}

// CmdUninstall removes the service, and with --purge the binary and the
// state directory as well.
func CmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the installed binary and the state directory (agent identity is lost)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := requireAdmin(); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
		return 1
	}

	if err := svc.Uninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
		return 1
	}
	fmt.Printf("service %q removed\n", svc.Name)

	if !*purge {
		fmt.Println("state directory and binary left in place; re-run with --purge to remove them")
		return 0
	}

	stateDir, err := config.Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: resolve state dir: %v\n", err)
		return 1
	}
	if err := removeTree(stateDir); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: remove %s: %v\n", stateDir, err)
		return 1
	}
	fmt.Printf("removed state directory: %s\n", stateDir)

	if p := svc.DefaultInstallPath(); p != "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			// The running binary is often the one being deleted. On Windows
			// that fails, which is expected rather than fatal.
			fmt.Fprintf(os.Stderr, "uninstall: could not remove %s: %v\n", p, err)
		} else {
			fmt.Printf("removed binary: %s\n", p)
		}
	}
	return 0
}

// CmdUpdateFinalize is the out-of-process half of a self-update. It waits for
// the old agent to exit, swaps the staged binary into place, and starts the
// service again. It is spawned by the updater on Windows, where the in-place
// rename can be refused; it is never meant to be run by hand.
//
// Every exit path writes an outcome to the update state file. The finalizer
// is the only witness to whether the swap happened: when it gave up silently,
// the host stayed on the old version while the console showed the update as
// applied.
func CmdUpdateFinalize(args []string) int {
	fs := flag.NewFlagSet("update-finalize", flag.ContinueOnError)
	pid := fs.Int("pid", 0, "pid of the agent process to wait for")
	target := fs.String("target", "", "path of the binary to replace")
	staged := fs.String("staged", "", "path of the verified replacement binary")
	stateDir := fs.String("state-dir", "", "agent state directory, for reporting the outcome")
	version := fs.String("version", "", "version being finalized, for the log line")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long to wait for the old process to exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *target == "" || *staged == "" {
		fmt.Fprintln(os.Stderr, "update-finalize: --target and --staged are required")
		return 2
	}

	dir := *stateDir
	if dir == "" {
		// A finalizer with no state dir cannot report anything, which is the
		// failure mode this whole function exists to close.
		if resolved, err := config.Dir(); err == nil {
			dir = resolved
		} else {
			fmt.Fprintf(os.Stderr, "update-finalize: resolve state dir: %v\n", err)
		}
	}
	report := func(failure error) {
		if dir == "" {
			return
		}
		if err := update.NewTracker(dir).FinalizeOutcome(failure); err != nil {
			fmt.Fprintf(os.Stderr, "update-finalize: record outcome: %v\n", err)
		}
	}

	ctx := context.Background()
	if *pid > 0 {
		if err := update.WaitForExit(ctx, *pid, *timeout); err != nil {
			// The old agent is still holding its image. Nothing was swapped,
			// so say so: the alternative is a host that never updates and a
			// server that thinks it did.
			report(fmt.Errorf("waiting for agent pid %d: %w", *pid, err))
			fmt.Fprintf(os.Stderr, "update-finalize: %v\n", err)
			return 1
		}
	}

	if _, err := update.Swap(*target, *staged); err != nil {
		report(fmt.Errorf("swap %s: %w", *target, err))
		fmt.Fprintf(os.Stderr, "update-finalize: %v\n", err)
		return 1
	}
	fmt.Printf("swapped %s (version %s)\n", *target, *version)
	// The swap is what the update was for, so record success before the
	// service start: a service that will not start is a rollback question,
	// not an "did the swap happen" question.
	report(nil)

	if err := svc.Start(); err != nil {
		// The service manager may already have restarted the agent through
		// its own recovery actions, which is a success, not a failure.
		fmt.Fprintf(os.Stderr, "update-finalize: start service: %v\n", err)
		return 1
	}
	fmt.Printf("service %q restarted\n", svc.Name)
	return 0
}

// installBinary copies the running executable to dest. When the running
// binary already is dest (a re-run of install, or a package managed install)
// the copy is skipped.
func installBinary(dest string) (string, error) {
	src, err := update.ExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve running binary: %w", err)
	}
	if sameFile(src, dest) {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	// Write beside the destination and rename in, so an interrupted install
	// cannot leave a truncated executable where the service expects one.
	tmp := dest + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy binary: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close %s: %w", tmp, err)
	}
	// Replacing a running Windows service binary needs the old file moved
	// aside first; on unix the rename alone is enough.
	if _, err := os.Stat(dest); err == nil && runtime.GOOS == "windows" {
		_ = os.Remove(update.BackupPath(dest))
		_ = os.Rename(dest, update.BackupPath(dest))
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("move %s into place: %w", dest, err)
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dest, err)
	}
	return dest, nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// removeTree deletes a directory tree after checking the path is a plausible
// state directory. A recursive delete driven by a computed path deserves a
// guard rail: an empty or root-ish value must never reach os.RemoveAll.
func removeTree(dir string) error {
	clean := filepath.Clean(strings.TrimSpace(dir))
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("refusing to delete %q", dir)
	}
	if vol := filepath.VolumeName(clean); vol != "" && clean == vol+string(filepath.Separator) {
		return fmt.Errorf("refusing to delete a volume root %q", dir)
	}
	if !strings.Contains(strings.ToLower(clean), "openrmm") {
		return fmt.Errorf("refusing to delete %q: it does not look like an openrmm state directory", clean)
	}
	if _, err := os.Stat(clean); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(clean)
}

// requireAdmin fails early with an actionable message rather than letting the
// first privileged syscall produce a bare "permission denied".
func requireAdmin() error {
	if runtime.GOOS == "windows" {
		// Windows privilege is checked by the SCM call itself, which returns
		// a clear access denied that we surface unchanged.
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("must run as root (try sudo)")
	}
	return nil
}

func printNextSteps(binary string) {
	fmt.Println()
	fmt.Println("next steps:")
	switch runtime.GOOS {
	case "linux":
		fmt.Println("  systemctl status openrmm-agent")
		fmt.Println("  journalctl -u openrmm-agent -f")
	case "darwin":
		fmt.Println("  sudo launchctl print system/com.openrmm.agent")
		fmt.Println("  tail -f /Library/Logs/OpenRMM/agent.err.log")
	case "windows":
		fmt.Println("  sc.exe query openrmm-agent")
		fmt.Println("  Get-EventLog -LogName Application -Source openrmm-agent -Newest 20")
	}
	fmt.Printf("  %s status\n", binary)
}
