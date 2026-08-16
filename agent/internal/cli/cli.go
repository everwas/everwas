// Package cli dispatches agent subcommands: run, enroll, status, version.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/rsp2k/openrmm/agent/internal/agentcore"
	"github.com/rsp2k/openrmm/agent/internal/config"
	"github.com/rsp2k/openrmm/agent/internal/conn"
	"github.com/rsp2k/openrmm/agent/internal/enroll"
	"github.com/rsp2k/openrmm/agent/internal/update"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

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

func cmdRun() int {
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
		log.Error("not enrolled; run `openrmm-agent enroll --server URL --token TOKEN` first")
		return 1
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A closed NATS connection is unrecoverable and silent: the agent keeps
	// running and heartbeating while being unable to receive anything. Dying
	// is the only way back, so the connection tells us and we exit non-zero
	// for the service manager to restart.
	deaf := make(chan struct{})
	var deafOnce sync.Once
	nc, err := conn.Connect(cfg, log, func() {
		deafOnce.Do(func() { close(deaf) })
	})
	if err != nil {
		log.Error("nats connect", "err", err)
		return 1
	}
	log.Info("agent started", "agent_id", cfg.AgentID, "version", Version, "nats_url", cfg.NATSURL)

	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	// A self-update swaps the binary on disk; only exiting makes the service
	// manager start it. The restart is deferred to here rather than done in
	// the update handler so the job's result is published first and the
	// shutdown path is the same one SIGTERM takes.
	restart := make(chan string, 1)
	var restartOnce sync.Once

	sup := &agentcore.Supervisor{
		NC:       nc,
		AgentID:  cfg.AgentID,
		Version:  Version,
		StateDir: stateDir,
		Log:      log,
		Restart: func(reason string) {
			restartOnce.Do(func() { restart <- reason })
		},
		RotateSecret: func(secret string) error {
			// Write the file before anything else believes the rotation
			// happened. The live NATS connection keeps working on the old
			// secret until it drops, and the server honours both for a
			// grace window, so there is no reconnect gap here.
			cfg.AgentSecret = secret
			return cfg.Save()
		},
	}
	sup.Start(ctx)

	why := waitForShutdown(ctx, deaf, restart)
	cancel()
	switch {
	case why.deaf:
		log.Error("nats connection is closed and cannot recover, exiting for restart")
	case why.restart != "":
		log.Info("restarting", "reason", why.restart)
	default:
		log.Info("shutting down")
	}
	sup.Wait()
	if why.deaf {
		// The conn is already closed; there is nothing to drain, and the exit
		// code is what makes the service manager act.
		return 1
	}
	if err := nc.Drain(); err != nil {
		log.Warn("nats drain", "err", err)
	}
	return 0
}

// stopReason says why the agent is shutting down. Zero value means it was
// asked to.
type stopReason struct {
	deaf    bool
	restart string
}

// waitForShutdown blocks until the agent should stop and reports why.
//
// A clean shutdown closes the conn itself, which fires the deaf callback, so
// being asked to stop always wins. The explicit ctx check comes first
// because a select with several ready cases picks at random, and a SIGTERM
// that arrives alongside the close must not be reported as a crash.
func waitForShutdown(ctx context.Context, deaf <-chan struct{}, restart <-chan string) stopReason {
	if ctx.Err() != nil {
		return stopReason{}
	}
	select {
	case <-ctx.Done():
		return stopReason{}
	case <-deaf:
		return stopReason{deaf: true}
	case reason := <-restart:
		return stopReason{restart: reason}
	}
}

func cmdStatus() int {
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
	fmt.Fprintln(os.Stderr, `usage: openrmm-agent <command>

commands:
  run         run the agent in the foreground
  enroll      enroll with a server: --server URL --token TOKEN
  install     install as a system service
  uninstall   remove the system service
  status      show agent status
  version     print version`)
}
