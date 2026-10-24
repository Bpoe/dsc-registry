package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Bpoe/dsc-registry/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cli.Version, cli.Commit = version, commit
	os.Exit(cli.Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
