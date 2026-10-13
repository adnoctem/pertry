// Command sicht sorts a large image archive: metadata and deterministic rules first, a local
// vision model for what the rules cannot decide, a human review pass for what the model was unsure
// about, and only then file movement, recorded in a manifest that can be undone.
//
// Nothing leaves the machine. This is a thin entrypoint; everything lives in internal/sicht.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/adnoctem/pertry/internal/sicht/cmd"

	// Imported for its link-time variables, which Bazel's x_defs populate.
	_ "github.com/adnoctem/pertry/pkg/stamp"
)

func main() {
	// os.Exit is called here and nowhere else, so that run() can use defer: os.Exit does not
	// run deferred functions, and the signal handler has to be unregistered.
	os.Exit(run())
}

func run() int {
	// A scan or an inference pass over several thousand images runs long enough that Ctrl-C
	// has to cancel it cleanly. Every stage writes per item, so a cancelled run keeps the
	// work it already did.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cmd.Execute(ctx, &cmd.App{Out: os.Stdout})
}
