// Package cli dispatches agent subcommands: run, enroll, status, version.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openrmm/agent/internal/agentcore"
	"github.com/openrmm/agent/internal/config"
	"github.com/openrmm/agent/internal/conn"
	"github.com/openrmm/agent/internal/enroll"
	"github.com/openrmm/agent/internal/update"
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
	cfg, err := config.Load()
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	if !cfg.Enrolled() {
		log.Error("not enrolled; run `openrmm-agent enroll --server URL --token TOKEN` first")
		return 1
	}

	stateDir, err := config.Dir()
	if err != nil {
		log.Error("resolve state dir", "err", err)
		return 1
	}

	// Before anything else: if the build we just updated to has crash looped,
	// put the previous binary back and exit so the service manager starts it.
	if rolledBack, err := update.CheckAndRollback(stateDir); err != nil {
		log.Warn("update rollback check", "err", err)
	} else if rolledBack {
		log.Error("update rolled back after repeated crashes, restarting previous version")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nc, err := conn.Connect(cfg, log)
	if err != nil {
		log.Error("nats connect", "err", err)
		return 1
	}
	log.Info("agent started", "agent_id", cfg.AgentID, "version", Version, "nats_url", cfg.NATSURL)

	sup := &agentcore.Supervisor{
		NC:       nc,
		AgentID:  cfg.AgentID,
		Version:  Version,
		StateDir: stateDir,
		Log:      log,
	}
	sup.Start(ctx)

	<-ctx.Done()
	log.Info("shutting down")
	sup.Wait()
	if err := nc.Drain(); err != nil {
		log.Warn("nats drain", "err", err)
	}
	return 0
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
