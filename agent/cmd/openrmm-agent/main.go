package main

import (
	"os"

	"github.com/openrmm/agent/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
