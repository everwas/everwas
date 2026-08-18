package main

import (
	"os"

	"github.com/everwas/everwas/agent/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
