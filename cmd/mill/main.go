// Command mill is a deterministic task-graph harness for coding agents.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/msidorkevich/mill/internal/cli"
)

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mill: %v\n", err)
		os.Exit(cli.ExitError)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Dir: dir}, os.Args[1:])
	stop()
	os.Exit(code)
}
