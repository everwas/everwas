package main

import (
	"os"

	"github.com/rsp2k/openrmm/agent/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
