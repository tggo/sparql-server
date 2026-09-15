// Command sparql-server is a SPARQL 1.1 endpoint in a single binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tggo/sparql-server/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal starts a graceful shutdown, restore the
		// default behaviour so that a second one terminates immediately.
		<-ctx.Done()
		stop()
	}()
	code := cli.Main(ctx, os.Args[1:], cli.Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Environ: os.Environ(),
	})
	stop()
	os.Exit(code)
}
