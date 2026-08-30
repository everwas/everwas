// Package cli dispatches agent subcommands: run, enroll, status, version.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/everwas/everwas/agent/internal/agentcore"
	"github.com/everwas/everwas/agent/internal/config"
	"github.com/everwas/everwas/agent/internal/conn"
	"github.com/everwas/everwas/agent/internal/enroll"
	"github.com/everwas/everwas/agent/internal/netcert"
	"github.com/everwas/everwas/agent/internal/secure"
	"github.com/everwas/everwas/agent/internal/svc"
	"github.com/everwas/everwas/agent/internal/update"
	"github.com/everwas/everwas/agent/internal/winident"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

// netcertDir is where the 802.1X key, certificate and chain live.
//
// A subdirectory of the state dir rather than the state dir itself, because a
// supplicant needs to read the certificate and chain while the private key
// stays owner-only, and keeping them together makes the eventual per-platform
// install step (wpa_supplicant, NetworkManager, the Windows certificate store)
// one directory to point at instead of three paths to keep in step.
func netcertDir(stateDir string) string {
	return filepath.Join(stateDir, "netcert")
}

func Run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println(Version)
		return 0
	case "enroll":
		return cmdEnroll(args[1:])
	case "run":
		return cmdRun()
	case "status":
		return cmdStatus()
	case "install":
		return CmdInstall(args[1:])
	case "uninstall":
		return CmdUninstall(args[1:])
	case "update-finalize":
		return CmdUpdateFinalize(args[1:])
	case "supplicant-profile":
		return CmdSupplicantProfile(args[1:])
	default:
		usage()
		return 2
	}
}

func cmdEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	server := fs.String("server", "", "server base URL, e.g. https://rmm.example.com")
	token := fs.String("token", "", "one-time enrollment token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "enroll: --server and --token are required")
		return 2
	}
	if err := enroll.Enroll(*server, *token, Version); err != nil {
		fmt.Fprintf(os.Stderr, "enroll: %v\n", err)
		return 1
	}
	path, _ := config.Path()
	fmt.Printf("enrolled; identity saved to %s\n", path)
	return 0
}

// cmdRun starts the agent, under the Windows service control dispatcher when
// the SCM launched us and directly otherwise.
//
// The split matters: a Windows service that never reports SERVICE_RUNNING is
// killed after 30 seconds with error 1053, however healthy it is. systemd has
// no such protocol, so this seam is invisible on Linux and the agent ran
// correctly there while being unstartable as a Windows service.
func cmdRun() int {
	if isService, err := svc.IsService(); err == nil && isService {
		return svc.RunAsService(svc.Name, runAgent)
	}
	// Interactive: Ctrl-C and SIGTERM are the stop signal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAgent(ctx)
}

// runAgent is the agent proper. Its parent context is whatever asked it to
// stop, so the shutdown path is identical either way.
func runAgent(parent context.Context) int {
	log := newLogger()

	// Rollback runs FIRST, before anything that can fail. A build that broke
	// config parsing crash loops on config.Load() forever, and a rollback
	// sitting below that line is never reached, fleet-wide, with the previous
	// good binary sitting right beside it. The state dir is resolvable without
	// reading any config, so this check has no prerequisites.
	stateDir, err := config.Dir()
	if err != nil {
		log.Error("resolve state dir", "err", err)
		return 1
	}
	// Migrate a pre-rename state directory BEFORE anything tries to read an
	// identity out of the new one. An agent installed as OpenRMM keeps its
	// credential, its 802.1X material and its schedule under the old path, and
	// a build that cannot see them reports "not enrolled" and exits, which for
	// a self-updated fleet means every machine goes dark at once and none can
	// phone home to be fixed.
	if moved, mErr := config.MigrateLegacyState(); mErr != nil {
		// Not fatal on its own: the agent may still be enrolled under the new
		// path, and failing to start over a migration that was not needed would
		// be worse than the migration not happening.
		log.Error("could not migrate the pre-rename state directory", "err", mErr)
	} else if moved {
		log.Info("migrated state from the pre-rename directory")
	}

	// Repair the state directory's permissions on every start, before anything
	// reads or writes a secret through it.
	//
	// Agents installed before this existed have a directory that inherited
	// C:\ProgramData's default ACL, so agent.json (the credential that
	// authenticates this device) and the 802.1X private key are readable by
	// every local user on the machine. Waiting for the next config write to
	// fix it would leave a laptop exposed until its credential happened to
	// rotate; a warning rather than a fatal because an agent that cannot fix
	// permissions is still better online than gone.
	if err := secure.MkdirAll(stateDir); err != nil {
		log.Warn("could not restrict the state directory; it may be readable by local users",
			"dir", stateDir, "err", err)
	}
	if rolledBack, rbErr := update.CheckAndRollback(stateDir); rbErr != nil {
		log.Warn("update rollback check", "err", rbErr)
	} else if rolledBack {
		log.Error("update rolled back after repeated crashes, restarting previous version")
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	if !cfg.Enrolled() {
		log.Error("not enrolled; run `everwas-agent enroll --server URL --token TOKEN` first")
		return 1
	}

	// Parsed at startup so a typo is reported NOW rather than being discovered
	// when somebody wonders why a migration never started. The loop re-reads
	// the value per cycle, because renewal rewrites it while the process runs;
	// this parse exists for the complaint and as the fallback when a later
	// re-read is unusable. An unusable value becomes auto, which is the one
	// that cannot take over a machine by accident.
	identityMode, err := winident.ParseMode(cfg.EffectiveNetworkIdentity())
	if err != nil {
		log.Error("network identity setting is not understood, using auto", "err", err)
	}

	// Renew BEFORE connecting, so the connection that follows is itself the
	// proof of receipt: the server clears the old secret the moment it sees
	// this agent authenticate with the new one.
	//
	// Renewing after connecting looked equivalent and was not. The live
	// connection keeps using the credential it dialled with, so the server
	// never observes the new one, rotation_in_flight stays true indefinitely,
	// and an operator pressing "rotate credentials" is refused until the agent
	// happens to reconnect. Observed on a live agent, not reasoned about.
	//
	// Bounded and non-fatal: the old credential still works, so a server that
	// is down or slow costs nothing here and the periodic loop tries again.
	renewCtx, cancelRenew := context.WithTimeout(parent, renewAtStartupTimeout)
	if err := enroll.Renew(renewCtx, cfg); err != nil {
		if errors.Is(err, enroll.ErrRenewRefused) {
			log.Error("credential renewal refused; this agent needs re-enrolling", "err", err)
		} else {
			log.Warn("credential renewal at startup failed, continuing on the current one", "err", err)
		}
	} else {
		log.Info("agent credential renewed at startup")
	}
	cancelRenew()

	// A closed NATS connection is unrecoverable and silent: the agent keeps
	// running and heartbeating while being unable to receive anything. Dying
	// is the only way back, so the connection tells us and we exit non-zero
	// for the service manager to restart.
	deaf := make(chan struct{})
	var deafOnce sync.Once
	// A permissions violation leaves the agent connected and unable to hear
	// anything on the denied subject. The health tracker below is what stops a
	// freshly updated build in that state from being confirmed healthy and
	// disarming its own rollback.
	health := agentcore.NewLinkHealth()
	nc, err := conn.Connect(cfg, log,
		func() { deafOnce.Do(func() { close(deaf) }) },
		health.RecordAsyncError,
	)
	if err != nil {
		log.Error("nats connect", "err", err)
		return 1
	}
	log.Info("agent started", "agent_id", cfg.AgentID, "version", Version, "nats_url", cfg.NATSURL)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// A self-update swaps the binary on disk; only exiting makes the service
	// manager start it. The restart is deferred to here rather than done in
	// the update handler so the job's result is published first and the
	// shutdown path is the same one SIGTERM takes.
	restart := make(chan stopReason, 1)
	var restartOnce sync.Once
	// Whether we have already said we are standing aside for another
	// identity provider. See the netcert closure below.
	var deferredOnce bool

	sup := &agentcore.Supervisor{
		NC:       nc,
		AgentID:  cfg.AgentID,
		Version:  Version,
		StateDir: stateDir,
		Log:      log,
		Link:     health,
		Restart: func(reason string) {
			restartOnce.Do(func() { restart <- stopReason{restart: reason} })
		},
		Handoff: func(reason string) {
			restartOnce.Do(func() { restart <- stopReason{handoff: reason} })
		},
		RenewCredential: func(ctx context.Context) error {
			return enroll.Renew(ctx, cfg)
		},
		RotateSecret: func(secret string) error {
			// Write the file before anything else believes the rotation
			// happened. The live NATS connection keeps working on the old
			// secret until it drops, and the server honours both for a
			// grace window, so there is no reconnect gap here.
			cfg.AgentSecret = secret
			return cfg.Save()
		},
		EnsureNetCert: func(ctx context.Context) (*netcert.Material, error) {
			// Read the secret at call time, not at wiring time. A rotation
			// replaces it while the process runs, and a closure that captured
			// the value at startup would keep presenting the old one and be
			// refused from the first rotation onwards, on a path that only
			// runs every twelve hours: nobody would connect the two.
			// Stand aside for a machine Active Directory is already
			// provisioning. Installing a second client-auth certificate makes
			// Windows' choice non-deterministic, and a netsh profile under a
			// Group Policy machine profile does nothing at all, so taking over
			// would not even be a clean takeover.
			//
			// A failed detection never defers: Decide is given what we did
			// find, which for a failure is nothing, and nothing never defers.
			// The dangerous direction here is stopping, not continuing.
			// The mode is re-read here for the same reason the secret above
			// is. Renewal rewrites the organization's policy on this machine
			// every twelve hours, and a mode captured at startup would mean a
			// fleet-wide change reached each machine only when it next
			// rebooted, which for a fleet of laptops is weeks. The value
			// parsed at startup stays the one that gets VALIDATED, so a typo
			// is still reported then rather than silently ignored now.
			mode, mErr := winident.ParseMode(cfg.EffectiveNetworkIdentity())
			if mErr != nil {
				mode = identityMode
			}
			if src, dErr := winident.Detect(ctx); dErr == nil {
				if d := winident.Decide(src, mode); d.Defer {
					if !deferredOnce {
						// Once, not every cycle. A machine that is correctly
						// deferring is in its steady state, and saying so twice
						// a day forever is how the line stops being read.
						//
						// Except when it is about to cost the machine its
						// network access, which is not a steady state and is
						// logged where somebody will see it.
						if d.Warn {
							log.Warn("not providing an 802.1X identity to this machine",
								"reason", d.Reason)
						} else {
							log.Info("not providing an 802.1X identity to this machine",
								"reason", d.Reason)
						}
						deferredOnce = true
					}
					// Reported the same way as a server with no CA: nothing is
					// wrong, there is no deadline, and the loop must not
					// escalate or interrupt anybody about it.
					return nil, netcert.ErrNotConfigured
				}
			}

			m, err := netcert.Ensure(
				ctx, netcertDir(stateDir), cfg.ServerURL, cfg.AgentID, cfg.AgentSecret, time.Now(),
			)
			// Only when one was actually obtained. This is the record that
			// this machine can authenticate to the network and until when,
			// which is the first thing anyone asks after a device stops
			// getting a DHCP lease.
			if err == nil && m.Issued {
				log.Info("network certificate issued",
					"serial", m.Serial,
					"not_after", m.NotAfter.Format(time.RFC3339),
					"path", m.CertPath)
			}
			// m is returned even on failure: it is whatever the device still
			// holds, and both the urgency of the failure and what we report to
			// the server are read from it.
			return m, err
		},
		WarnUserAboutCertificate: warnUserAboutCertificate(stateDir, log),
	}
	sup.Start(ctx)

	why := waitForShutdown(ctx, deaf, restart)
	cancel()
	switch {
	case why.deaf:
		log.Error("nats connection is closed and cannot recover, exiting for restart")
	case why.restart != "":
		log.Info("restarting", "reason", why.restart)
	case why.handoff != "":
		log.Info("exiting for the update finalizer", "reason", why.handoff)
	default:
		log.Info("shutting down")
	}
	sup.Wait()
	if why.deaf {
		// The conn is already closed; there is nothing to drain, and the exit
		// code is what makes the service manager act.
		return exitCodeFor(why)
	}
	if err := nc.Drain(); err != nil {
		log.Warn("nats drain", "err", err)
	}
	return exitCodeFor(why)
}

// Exit codes. Only "asked to stop" is zero.
//
// The service manager reads these, and on Windows it reads them to decide
// whether to start the agent again at all: the SCM runs its recovery actions
// on a non-zero exit and treats zero as "finished, leave it stopped". A
// self-update that exited 0 therefore left the host with no agent until the
// next reboot. StartType=Automatic does not help; it only applies at boot.
//
// Neither code feeds the rollback crash counters, which count process starts
// inside a window and never inspect an exit code.
// renewAtStartupTimeout bounds the pre-connect renewal. Short: the agent has a
// working credential already, so waiting here only delays it becoming useful.
const renewAtStartupTimeout = 20 * time.Second

const (
	// exitRestart: the binary on disk changed and only exiting picks it up.
	exitRestart = 10
	// exitDeaf: the NATS connection is closed and cannot recover, so the
	// process is alive and unable to be told anything.
	exitDeaf = 11
)

// exitCodeFor maps a shutdown reason to the code the service manager sees.
func exitCodeFor(why stopReason) int {
	switch {
	case why.deaf:
		return exitDeaf
	case why.restart != "":
		return exitRestart
	case why.handoff != "":
		// Zero on purpose: the finalizer restarts the service after it swaps.
		return 0
	default:
		return 0
	}
}

// stopReason says why the agent is shutting down. Zero value means it was
// asked to.
type stopReason struct {
	deaf    bool
	restart string
	// handoff is a self-update that could not swap in place and handed the
	// swap to a helper process. That helper is BLOCKED waiting for this
	// process to exit, and it starts the service itself once the swap is
	// done, so this exit must be clean: a non-zero exit would have the SCM
	// restart us into the old binary at +5s, racing the finalizer, and
	// whichever won, the host would be running the old code with the new
	// binary sitting unused on disk.
	handoff string
}

// waitForShutdown blocks until the agent should stop and reports why.
//
// A clean shutdown closes the conn itself, which fires the deaf callback, so
// being asked to stop always wins. The explicit ctx check comes first
// because a select with several ready cases picks at random, and a SIGTERM
// that arrives alongside the close must not be reported as a crash.
func waitForShutdown(ctx context.Context, deaf <-chan struct{}, restart <-chan stopReason) stopReason {
	if ctx.Err() != nil {
		return stopReason{}
	}
	select {
	case <-ctx.Done():
		return stopReason{}
	case <-deaf:
		return stopReason{deaf: true}
	case why := <-restart:
		return why
	}
}

func cmdStatus() int {
	// Same migration as the run path. Without it the first thing an operator
	// runs after a failed upgrade confirms the wrong diagnosis.
	if _, err := config.MigrateLegacyState(); err != nil {
		fmt.Fprintf(os.Stderr, "status: could not migrate pre-rename state: %v\n", err)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		return 1
	}
	path, _ := config.Path()
	if !cfg.Enrolled() {
		fmt.Printf("not enrolled (state file: %s)\n", path)
		return 0
	}
	fmt.Printf("enrolled\n  agent_id: %s\n  server:   %s\n  nats_url: %s\n  state:    %s\n",
		cfg.AgentID, cfg.ServerURL, cfg.NATSURL, path)
	return 0
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: everwas-agent <command>

commands:
  run         run the agent in the foreground
  enroll      enroll with a server: --server URL --token TOKEN
  install     install as a system service
  uninstall   remove the system service
  status      show agent status
  version     print version

  supplicant-profile   write an 802.1X client profile for this device
                       (--ssid NAME for wireless; omit for wired). Writes a
                       file and nothing else: starting a supplicant against
                       it stays a deliberate decision.`)
}
