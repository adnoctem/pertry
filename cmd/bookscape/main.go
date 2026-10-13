// Command bookscape ingests Netscape bookmark exports, deduplicates and enriches them in
// SQLite, and re-emits a bookmark file for import into Linkwarden.
//
// This is a thin entrypoint. Everything lives in internal/bookscape.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/adnoctem/pertry/internal/bookscape/cmd"

	// Imported for its link-time variables, which Bazel's x_defs populate.
	_ "github.com/adnoctem/pertry/pkg/stamp"
)

func main() {
	// os.Exit is called here and nowhere else, so that run() can use defer: os.Exit does
	// not run deferred functions, and the signal handler has to be unregistered.
	os.Exit(run())
}

func run() int {
	// A liveness or classification pass over several thousand links is long enough that
	// Ctrl-C has to cancel it cleanly: both stages persist per result, so a cancelled run
	// keeps the work it already did.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cmd.Execute(ctx, &cmd.App{Out: os.Stdout})
}
