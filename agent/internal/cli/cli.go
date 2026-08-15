// Package cli dispatches agent subcommands: run, enroll, install, version.
package cli

import (
	"fmt"
	"os"
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
	case "run", "enroll", "install", "uninstall", "status":
		fmt.Fprintf(os.Stderr, "openrmm-agent %s: not implemented yet (M1)\n", args[0])
		return 1
	default:
		usage()
		return 2
	}
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
